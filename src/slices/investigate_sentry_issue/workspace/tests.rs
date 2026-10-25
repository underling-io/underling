//! The workspace against real git and a local bare remote.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use tempfile::TempDir;
use tokio_util::sync::CancellationToken;

use super::git::GitRunner;
use super::*;

const TOKEN: &str = "ghp-test-token-4f2a9c";
const BRANCH: &str = "heinzel/sentry-1";

/// Runs git in `dir` and returns its trimmed output.
fn git(dir: impl AsRef<Path>, args: &[&str]) -> String {
    let output = Command::new("git")
        .args(["-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"])
        .args(args)
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(output.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&output.stderr));
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

fn path_var() -> String {
    std::env::var("PATH").unwrap()
}

fn s(path: &Path) -> String {
    path.to_str().unwrap().to_owned()
}

/// A bare repo whose `main` branch has one commit, in `root`.
fn test_remote(root: &TempDir) -> String {
    let remote = root.path().join("web.git");
    let seed = root.path().join("seed");
    fs::create_dir(&seed).unwrap();
    git(&seed, &["init", "-q", "--bare", "-b", "main", &s(&remote)]);
    git(&seed, &["init", "-q", "-b", "main"]);
    fs::write(seed.join("README.md"), "web\n").unwrap();
    git(&seed, &["add", "README.md"]);
    git(&seed, &["commit", "-q", "-m", "Initial commit"]);
    git(&seed, &["push", "-q", &s(&remote), "main"]);
    s(&remote)
}

/// A workspace on a fresh test remote, in a private work dir.
struct Fixture {
    ws: Workspace,
    remote: String,
    _root: TempDir,
}

async fn new_test_workspace() -> Fixture {
    let root = TempDir::new().unwrap();
    let remote = test_remote(&root);
    let work_dir = s(&root.path().join("work"));
    let ws = Workspace::new(&work_dir, &remote, BRANCH, &path_var(), TOKEN, &CancellationToken::new())
        .await
        .unwrap();
    Fixture { ws, remote, _root: root }
}

/// Writes `file` in `dir` and commits it on the current branch, as Claude
/// would.
fn commit(dir: &str, file: &str) {
    let path = Path::new(dir).join(file);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(&path, "fixed\n").unwrap();
    git(dir, &["add", file]);
    git(dir, &["commit", "-q", "-m", &format!("Fix {file}")]);
}

/// Runs `stray_branches`, `new_commits` and, if there are commits,
/// `changed_paths`, in the order the investigation does.
async fn inspect(ws: &mut Workspace) -> (Vec<String>, u64) {
    let cancel = CancellationToken::new();
    let stray = ws.stray_branches(&cancel).await.unwrap();
    let commits = ws.new_commits(&cancel).await.unwrap();
    if commits > 0 {
        ws.changed_paths(&cancel).await.unwrap();
    }
    (stray, commits)
}

fn remote_has_branch(remote: &str) -> bool {
    Command::new("git")
        .args(["rev-parse", "--verify", "-q", &format!("refs/heads/{BRANCH}")])
        .current_dir(remote)
        .output()
        .unwrap()
        .status
        .success()
}

/// The forms in which `token` could leak to disk: raw, as the base64 git's
/// Authorization header carries, and as bare base64.
fn token_forms(token: &str) -> [String; 3] {
    [
        token.to_owned(),
        BASE64.encode(format!("x-access-token:{token}")),
        BASE64.encode(token),
    ]
}

fn entries(dir: &str) -> Vec<String> {
    let mut names: Vec<String> = fs::read_dir(dir)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    names
}

#[tokio::test]
async fn new_checks_out_the_branch_from_the_default_branch() {
    let f = new_test_workspace().await;

    assert_eq!(git(&f.ws.clone, &["rev-parse", "--abbrev-ref", "HEAD"]), BRANCH);
    assert_eq!(f.ws.default_branch, "main");
    for dir in [&f.ws.home, &f.ws.claude_config, &f.ws.git_home] {
        let meta = fs::metadata(dir).unwrap();
        assert!(meta.is_dir(), "{dir}");
        assert_eq!(meta.permissions().mode() & 0o777, 0o700, "{dir}");
        assert_eq!(Path::new(dir).parent().unwrap(), Path::new(&f.ws.run_dir));
    }
    assert_eq!(f.ws.run_dir, s(&fs::canonicalize(&f.ws.run_dir).unwrap()));
    assert_eq!(git(&f.ws.clone, &["config", "--local", "user.name"]), "heinzel");
    assert_eq!(git(&f.ws.clone, &["config", "--local", "user.email"]), "heinzel@users.noreply.github.com");
    assert_eq!(git(&f.ws.clone, &["config", "--local", "commit.gpgsign"]), "false");
}

#[tokio::test]
async fn new_fails_and_cleans_up_when_the_clone_fails() {
    let root = TempDir::new().unwrap();
    let work_dir = s(&root.path().join("work"));
    let missing = s(&root.path().join("missing.git"));

    let err = Workspace::new(&work_dir, &missing, BRANCH, &path_var(), TOKEN, &CancellationToken::new())
        .await
        .unwrap_err();

    assert!(err.0.starts_with("git clone: exit status 128: "), "{err}");
    assert!(entries(&work_dir).is_empty());
}

#[tokio::test]
async fn new_lists_the_work_dir_s_other_entries_as_siblings() {
    let root = TempDir::new().unwrap();
    let remote = test_remote(&root);
    let work_dir = root.path().join("work");
    fs::create_dir_all(work_dir.join("old-run-b")).unwrap();
    fs::create_dir_all(work_dir.join("old-run-a")).unwrap();
    fs::set_permissions(&work_dir, fs::Permissions::from_mode(0o700)).unwrap();

    let ws = Workspace::new(&s(&work_dir), &remote, BRANCH, &path_var(), TOKEN, &CancellationToken::new())
        .await
        .unwrap();

    assert_eq!(
        ws.siblings,
        [format!("{}/old-run-a", ws.work_dir), format!("{}/old-run-b", ws.work_dir)]
    );
}

#[tokio::test]
async fn new_rejects_a_work_dir_others_can_use() {
    let root = TempDir::new().unwrap();
    let remote = test_remote(&root);
    let work_dir = root.path().join("work");
    fs::create_dir(&work_dir).unwrap();
    fs::set_permissions(&work_dir, fs::Permissions::from_mode(0o777)).unwrap();

    let err = Workspace::new(&s(&work_dir), &remote, BRANCH, &path_var(), TOKEN, &CancellationToken::new())
        .await
        .unwrap_err();

    assert!(err.0.contains("work dir"), "{err}");
    assert!(entries(&s(&work_dir)).is_empty());
}

#[tokio::test]
async fn remove_deletes_the_run_dir() {
    let f = new_test_workspace().await;

    f.ws.remove().await.unwrap();

    assert!(entries(&f.ws.work_dir).is_empty());
}

#[tokio::test]
async fn the_clone_shares_no_object_files_with_the_mirror() {
    use std::os::unix::fs::MetadataExt;
    let f = new_test_workspace().await;

    let objects = Path::new(&f.ws.mirror).join("objects");
    let mut compared = 0;
    let mut stack = vec![objects.clone()];
    while let Some(dir) = stack.pop() {
        for entry in fs::read_dir(dir).unwrap() {
            let path = entry.unwrap().path();
            if path.is_dir() {
                stack.push(path);
                continue;
            }
            let rel = path.strip_prefix(&objects).unwrap();
            if let Ok(clone) = fs::metadata(Path::new(&f.ws.clone).join(".git/objects").join(rel)) {
                compared += 1;
                let mirror = fs::metadata(&path).unwrap();
                assert!(
                    (mirror.dev(), mirror.ino()) != (clone.dev(), clone.ino()),
                    "object {} is hard-linked between the mirror and the clone",
                    rel.display()
                );
            }
        }
    }
    assert!(compared > 0, "found no object files to compare");
}

#[tokio::test]
async fn a_commit_on_the_branch_is_counted_and_pushed() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, "fix.txt");

    let (stray, commits) = inspect(&mut f.ws).await;
    assert!(stray.is_empty());
    assert_eq!(commits, 1);
    let changed = f.ws.changed_paths(&CancellationToken::new()).await.unwrap();
    assert_eq!(changed, ["fix.txt"]);
    f.ws.push(&CancellationToken::new()).await.unwrap();

    assert_eq!(
        git(&f.remote, &["log", "-1", "--format=%s", &format!("refs/heads/{BRANCH}")]),
        "Fix fix.txt"
    );
}

#[tokio::test]
async fn no_commits_count_as_none() {
    let mut f = new_test_workspace().await;

    assert_eq!(inspect(&mut f.ws).await, (vec![], 0));
}

#[tokio::test]
async fn changed_paths_list_a_rename_as_a_deletion_and_an_addition() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, ".github/workflows/ci.yml");
    git(&f.ws.clone, &["mv", "README.md", "MOVED.md"]);
    git(&f.ws.clone, &["commit", "-q", "-m", "Move"]);
    inspect(&mut f.ws).await;

    let changed = f.ws.changed_paths(&CancellationToken::new()).await.unwrap();

    assert_eq!(changed, [".github/workflows/ci.yml", "MOVED.md", "README.md"]);
}

#[tokio::test]
async fn commits_on_another_branch_are_stray() {
    let mut f = new_test_workspace().await;
    git(&f.ws.clone, &["checkout", "-q", "-b", "claude-own"]);
    commit(&f.ws.clone, "fix.txt");

    let (stray, _) = inspect(&mut f.ws).await;

    assert_eq!(stray, ["claude-own"]);
}

#[tokio::test]
async fn commits_on_a_renamed_branch_are_stray() {
    let mut f = new_test_workspace().await;
    git(&f.ws.clone, &["branch", "-m", "claude-renamed"]);
    commit(&f.ws.clone, "fix.txt");

    let (stray, _) = inspect(&mut f.ws).await;

    assert_eq!(stray, ["claude-renamed"]);
}

#[tokio::test]
async fn a_deleted_branch_without_commits_has_none() {
    let mut f = new_test_workspace().await;
    git(&f.ws.clone, &["checkout", "-q", "main"]);
    git(&f.ws.clone, &["branch", "-q", "-D", BRANCH]);

    assert_eq!(inspect(&mut f.ws).await, (vec![], 0));
}

#[tokio::test]
async fn a_helper_branch_merged_into_the_branch_is_not_stray() {
    let mut f = new_test_workspace().await;
    git(&f.ws.clone, &["checkout", "-q", "-b", "claude-own"]);
    commit(&f.ws.clone, "fix.txt");
    git(&f.ws.clone, &["checkout", "-q", BRANCH]);
    git(&f.ws.clone, &["merge", "-q", "--ff-only", "claude-own"]);

    assert_eq!(inspect(&mut f.ws).await, (vec![], 1));
}

#[tokio::test]
async fn the_child_s_git_config_does_not_affect_the_push() {
    let mut f = new_test_workspace().await;
    // Redirect the GitHub remote through the child's HOME, which heinzel's
    // own git must not read.
    let hostile = format!(
        "[url \"{}/nowhere.git\"]\n\tinsteadOf = {}\n",
        f.ws.run_dir, f.remote
    );
    fs::write(Path::new(&f.ws.home).join(".gitconfig"), hostile).unwrap();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;

    f.ws.push(&CancellationToken::new()).await.unwrap();

    assert!(remote_has_branch(&f.remote), "the push did not reach the remote");
}

#[tokio::test]
async fn push_runs_with_hooks_off() {
    let mut f = new_test_workspace().await;
    let marker = Path::new(&f.ws.run_dir).join("pre-push-ran");
    let hook = Path::new(&f.ws.mirror).join("hooks/pre-push");
    fs::write(&hook, format!("#!/bin/sh\ntouch {}\n", marker.display())).unwrap();
    fs::set_permissions(&hook, fs::Permissions::from_mode(0o755)).unwrap();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;

    f.ws.push(&CancellationToken::new()).await.unwrap();

    assert!(remote_has_branch(&f.remote));
    assert!(!marker.exists(), "the mirror's pre-push hook ran");
}

/// Plants `hook` in the mirror; it touches a marker file the test can
/// check.
fn plant_mirror_hook(ws: &Workspace, hook: &str) -> std::path::PathBuf {
    let marker = Path::new(&ws.run_dir).join(format!("{hook}-ran"));
    let path = Path::new(&ws.mirror).join("hooks").join(hook);
    fs::write(&path, format!("#!/bin/sh\ntouch {}\n", marker.display())).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
    marker
}

#[tokio::test]
async fn no_mirror_hook_runs_while_inspecting_and_pushing() {
    let mut f = new_test_workspace().await;
    let markers: Vec<_> = ["reference-transaction", "post-checkout", "pre-push", "push-to-checkout"]
        .into_iter()
        .map(|hook| plant_mirror_hook(&f.ws, hook))
        .collect();
    commit(&f.ws.clone, "fix.txt");

    inspect(&mut f.ws).await;
    f.ws.changed_paths(&CancellationToken::new()).await.unwrap();
    f.ws.push(&CancellationToken::new()).await.unwrap();

    assert!(remote_has_branch(&f.remote));
    for marker in markers {
        assert!(!marker.exists(), "{} ran", marker.display());
    }
}

#[tokio::test]
async fn the_push_goes_to_the_repo_s_url_not_a_remote() {
    let mut f = new_test_workspace().await;
    let attacker = TempDir::new().unwrap();
    git(attacker.path(), &["init", "-q", "--bare"]);
    git(&f.ws.mirror, &["config", "remote.origin.pushurl", &s(attacker.path())]);
    git(&f.ws.mirror, &["config", "remote.origin.url", &s(attacker.path())]);
    // Accept the change, to see where the push itself goes.
    f.ws.mirror_config = f.ws.read_mirror_config().unwrap();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;

    f.ws.push(&CancellationToken::new()).await.unwrap();

    assert!(remote_has_branch(&f.remote));
    assert!(!remote_has_branch(&s(attacker.path())), "the push went to the attacker");
}

#[tokio::test]
async fn a_changed_mirror_config_stops_the_push() {
    let mut f = new_test_workspace().await;
    f.ws.check_mirror().unwrap();
    git(&f.ws.mirror, &["config", "url./nowhere.insteadOf", &f.remote]);

    let checked = f.ws.check_mirror();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;
    let pushed = f.ws.push(&CancellationToken::new()).await;

    assert_eq!(
        checked,
        Err(Error("the mirror changed while claude ran (config), so heinzel won't push from it".into()))
    );
    assert_eq!(
        pushed,
        Err(Error("the mirror's git config changed, so heinzel won't push from it".into()))
    );
    assert!(!remote_has_branch(&f.remote));
}

#[tokio::test]
async fn anything_added_to_the_mirror_fails_the_check() {
    let f = new_test_workspace().await;
    fs::write(Path::new(&f.ws.mirror).join("commondir"), "/elsewhere\n").unwrap();

    assert_eq!(
        f.ws.check_mirror(),
        Err(Error("the mirror changed while claude ran (commondir), so heinzel won't push from it".into()))
    );
}

#[tokio::test]
async fn the_mirror_is_read_only_while_locked() {
    let f = new_test_workspace().await;

    f.ws.lock_mirror().unwrap();
    let added = fs::write(Path::new(&f.ws.mirror).join("commondir"), "/elsewhere\n");
    let hooked = fs::write(Path::new(&f.ws.mirror).join("hooks/pre-push"), "#!/bin/sh\n");
    f.ws.unlock_mirror();

    assert_eq!(added.unwrap_err().kind(), std::io::ErrorKind::PermissionDenied);
    assert_eq!(hooked.unwrap_err().kind(), std::io::ErrorKind::PermissionDenied);
    f.ws.check_mirror().unwrap();
}

#[tokio::test]
async fn a_locked_mirror_can_still_be_removed() {
    let f = new_test_workspace().await;
    f.ws.lock_mirror().unwrap();

    f.ws.remove().await.unwrap();

    assert!(!Path::new(&f.ws.run_dir).exists());
}

// The reviewer's hostile mirrors. Each test bypasses the checks that would
// stop the run first, to show the guard it names holds on its own.

/// A bare repo whose config would send pushes to `remote` to an attacker's
/// repo, and turn fetches from `clone` into running a command that touches
/// `marker`. Returns its path and the attacker's.
fn evil_repo(root: &Path, remote: &str, clone: &str, marker: &Path) -> (String, String) {
    let evil = s(&root.join("evil.git"));
    let attacker = s(&root.join("attacker.git"));
    git(root, &["init", "-q", "--bare", &evil]);
    git(root, &["init", "-q", "--bare", &attacker]);
    git(&evil, &["config", &format!("url.{attacker}.insteadOf"), remote]);
    git(&evil, &["config", "protocol.ext.allow", "always"]);
    git(&evil, &["config", &format!("url.ext::sh -c touch% {}.insteadOf", marker.display()), clone]);
    (evil, attacker)
}

/// Pushes the branch from the mirror as `push` does, minus its checks.
async fn push_unchecked(ws: &Workspace) -> Result<String, Error> {
    let r = format!("refs/heads/{BRANCH}");
    ws.in_mirror(true, &["push", "--quiet", "--no-verify", &ws.remote_url, &format!("{r}:{r}")], &CancellationToken::new())
        .await
}

#[tokio::test]
async fn a_commondir_in_the_mirror_points_git_nowhere_else() {
    let mut f = new_test_workspace().await;
    let root = TempDir::new().unwrap();
    let marker = root.path().join("ext-ran");
    let (evil, attacker) = evil_repo(root.path(), &f.remote, &f.ws.clone, &marker);
    fs::write(Path::new(&f.ws.mirror).join("commondir"), format!("{evil}\n")).unwrap();
    commit(&f.ws.clone, "fix.txt");

    let inspected = f.ws.stray_branches(&CancellationToken::new()).await;
    let counted = f.ws.new_commits(&CancellationToken::new()).await;
    f.ws.changed_paths(&CancellationToken::new()).await.unwrap();
    let pushed = push_unchecked(&f.ws).await;
    let checked = f.ws.push(&CancellationToken::new()).await;

    assert!(!marker.exists(), "a fetch ran the evil repo's ext:: command");
    assert_eq!((inspected, counted), (Ok(vec![]), Ok(1)));
    pushed.unwrap();
    assert!(remote_has_branch(&f.remote));
    assert!(!remote_has_branch(&attacker), "the push went to the attacker");
    assert_eq!(checked, Err(Error("the mirror holds commondir, so heinzel won't push from it".into())));
}

#[tokio::test]
async fn a_git_file_in_the_mirror_points_git_nowhere_else() {
    let mut f = new_test_workspace().await;
    let root = TempDir::new().unwrap();
    let marker = root.path().join("ext-ran");
    let (evil, attacker) = evil_repo(root.path(), &f.remote, &f.ws.clone, &marker);
    fs::write(Path::new(&f.ws.mirror).join(".git"), format!("gitdir: {evil}\n")).unwrap();
    commit(&f.ws.clone, "fix.txt");

    let inspected = f.ws.stray_branches(&CancellationToken::new()).await;
    let counted = f.ws.new_commits(&CancellationToken::new()).await;
    f.ws.changed_paths(&CancellationToken::new()).await.unwrap();
    let pushed = push_unchecked(&f.ws).await;
    let checked = f.ws.push(&CancellationToken::new()).await;

    assert!(!marker.exists(), "a fetch ran the evil repo's ext:: command");
    assert_eq!((inspected, counted), (Ok(vec![]), Ok(1)));
    pushed.unwrap();
    assert!(remote_has_branch(&f.remote));
    assert!(!remote_has_branch(&attacker), "the push went to the attacker");
    assert_eq!(checked, Err(Error("the mirror holds .git, so heinzel won't push from it".into())));
}

#[tokio::test]
async fn a_replace_ref_in_the_mirror_hides_nothing() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, ".github/workflows/ci.yml");
    inspect(&mut f.ws).await;
    let tip = git(&f.ws.mirror, &["rev-parse", &format!("refs/heads/{BRANCH}")]);
    let replace = Path::new(&f.ws.mirror).join("refs/replace");
    fs::create_dir_all(&replace).unwrap();
    fs::write(replace.join(&tip), format!("{}\n", f.ws.base)).unwrap();

    let changed = f.ws.changed_paths(&CancellationToken::new()).await;
    let checked = f.ws.push(&CancellationToken::new()).await;

    assert_eq!(changed, Ok(vec![".github/workflows/ci.yml".to_owned()]));
    assert_eq!(checked, Err(Error("the mirror holds refs/replace, so heinzel won't push from it".into())));
    assert!(!remote_has_branch(&f.remote));
}

#[tokio::test]
async fn the_ext_protocol_never_runs_in_the_mirror() {
    let mut f = new_test_workspace().await;
    let root = TempDir::new().unwrap();
    let marker = root.path().join("ext-ran");
    git(&f.ws.mirror, &["config", "protocol.ext.allow", "always"]);
    git(&f.ws.mirror, &["config", &format!("url.ext::sh -c touch% {}.insteadOf", marker.display()), &f.ws.clone]);

    let inspected = f.ws.stray_branches(&CancellationToken::new()).await;

    assert!(!marker.exists(), "a fetch ran an ext:: command");
    let err = inspected.unwrap_err().0;
    assert!(err.contains("transport 'ext' not allowed"), "{err}");
}

#[tokio::test]
async fn a_symlink_in_the_mirror_stops_the_push() {
    let mut f = new_test_workspace().await;
    std::os::unix::fs::symlink("/etc", Path::new(&f.ws.mirror).join("info/elsewhere")).unwrap();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;

    let pushed = f.ws.push(&CancellationToken::new()).await;

    assert_eq!(
        pushed,
        Err(Error("the mirror holds a symlink at info/elsewhere, so heinzel won't push from it".into()))
    );
}

#[tokio::test]
async fn the_push_sends_the_commit_heinzel_checked() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;
    let checked = git(&f.ws.clone, &["rev-parse", "HEAD"]);
    // The branch moves on to a CI change after the check.
    commit(&f.ws.clone, ".github/workflows/ci.yml");
    f.ws.new_commits(&CancellationToken::new()).await.unwrap();
    f.ws.checked_manifest = f.ws.take_mirror_manifest().unwrap();

    f.ws.push(&CancellationToken::new()).await.unwrap();

    let pushed = git(&f.remote, &["rev-parse", &format!("refs/heads/{BRANCH}")]);
    assert_eq!(pushed, checked);
}

#[tokio::test]
async fn a_mirror_change_after_the_check_stops_the_push() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;
    fs::write(Path::new(&f.ws.mirror).join("info/extra"), "x").unwrap();

    let pushed = f.ws.push(&CancellationToken::new()).await;

    assert_eq!(
        pushed,
        Err(Error("the mirror changed after heinzel checked it (info/extra), so heinzel won't push from it".into()))
    );
    assert!(!remote_has_branch(&f.remote));
}

#[tokio::test]
async fn nothing_is_pushed_before_the_changes_were_checked() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, "fix.txt");
    f.ws.stray_branches(&CancellationToken::new()).await.unwrap();
    f.ws.new_commits(&CancellationToken::new()).await.unwrap();

    let pushed = f.ws.push(&CancellationToken::new()).await;

    assert!(pushed.is_err());
    assert!(!remote_has_branch(&f.remote));
}

#[tokio::test]
async fn removing_a_run_dir_that_is_gone_succeeds() {
    let f = new_test_workspace().await;
    f.ws.remove().await.unwrap();

    f.ws.remove().await.unwrap();
}

#[tokio::test]
async fn push_fails_when_the_remote_rejects_it() {
    let mut f = new_test_workspace().await;
    let hook = Path::new(&f.remote).join("hooks/pre-receive");
    fs::write(&hook, "#!/bin/sh\necho rejected by test >&2\nexit 1\n").unwrap();
    fs::set_permissions(&hook, fs::Permissions::from_mode(0o755)).unwrap();
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;

    let err = f.ws.push(&CancellationToken::new()).await.unwrap_err();

    assert!(err.0.starts_with("git push: exit status 1: "), "{err}");
    assert!(err.0.contains("rejected by test"), "{err}");
}

#[tokio::test]
async fn the_token_is_in_neither_git_config_after_a_push() {
    let mut f = new_test_workspace().await;
    commit(&f.ws.clone, "fix.txt");
    inspect(&mut f.ws).await;
    f.ws.push(&CancellationToken::new()).await.unwrap();

    for file in [format!("{}/.git/config", f.ws.clone), format!("{}/config", f.ws.mirror)] {
        let data = fs::read_to_string(&file).unwrap();
        for form in token_forms(TOKEN) {
            assert!(!data.contains(&form), "{file} holds the token as {form:?}:\n{data}");
        }
    }
}

#[tokio::test]
async fn git_errors_redact_the_token() {
    let root = TempDir::new().unwrap();
    let g = GitRunner::new(&path_var(), &s(root.path()), TOKEN);
    let missing = s(&root.path().join(format!("{TOKEN}.git")));

    let err = g
        .run(&s(root.path()), false, &["clone", "--quiet", &missing, "x"], &CancellationToken::new())
        .await
        .unwrap_err();

    assert!(!err.0.contains(TOKEN), "{err}");
    assert!(err.0.contains("[redacted]"), "{err}");
}

#[test]
fn git_errors_redact_every_form_of_the_token() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", TOKEN);
    let [raw, header, bare] = token_forms(TOKEN);
    let output = format!("a {raw} b Authorization: Basic {header} c {bare} d x-access-token:{raw}");

    let got = g.redact(&output);

    assert_eq!(got, "a [redacted] b Authorization: Basic [redacted] c [redacted] d x-access-token:[redacted]");
}

#[test]
fn a_token_straddling_the_cut_is_redacted() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", TOKEN);
    // The tail keeps the last 2048 bytes; the token starts 5 bytes before
    // that cut.
    let output = format!("{}{TOKEN}{}", "a".repeat(100), "b".repeat(2048 - TOKEN.len() + 5));

    let got = g.redact(&output);

    assert_eq!(got.len(), 2048);
    assert!(!got.contains(&TOKEN[5..]), "{}", &got[..40]);
    assert!(got.contains("[redacted]"), "{}", &got[..40]);
}

#[test]
fn a_token_cut_off_at_the_end_is_redacted() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", TOKEN);

    assert_eq!(g.redact(&format!("fatal: {}", &TOKEN[..10])), "fatal: [redacted]");
    assert_eq!(g.redact("fatal: ghp-te"), "fatal: ghp-te", "too short to tell");
}

#[test]
fn debug_output_hides_the_token() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", TOKEN);

    assert!(!format!("{g:?}").contains(TOKEN));
}

#[test]
fn git_timeouts_depend_on_the_subcommand() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", "token");
    let cases: [(&[&str], Duration); 5] = [
        (&["clone", "--quiet", "--bare", "https://github.com/acme/web.git", "/w/run/mirror.git"], Duration::from_secs(600)),
        (&["fetch", "--quiet", "/w/run/repo", "+refs/heads/x:refs/heads/x"], Duration::from_secs(600)),
        (&["-c", "core.hooksPath=/dev/null", "push", "--quiet", "origin", "x"], Duration::from_secs(600)),
        (&["rev-list", "--count", "a..b"], Duration::from_secs(60)),
        (&["-c", "x=y", "config", "user.name", "heinzel"], Duration::from_secs(60)),
    ];
    for (args, want) in cases {
        assert_eq!(g.timeout(args), want, "{args:?}");
    }
}

#[tokio::test]
async fn the_git_runner_kills_commands_after_their_timeout() {
    // A git that hangs, so the timeout always comes first.
    let root = TempDir::new().unwrap();
    let bin = root.path().join("bin");
    fs::create_dir(&bin).unwrap();
    fs::write(bin.join("git"), "#!/bin/sh\nexec sleep 10\n").unwrap();
    fs::set_permissions(bin.join("git"), fs::Permissions::from_mode(0o755)).unwrap();
    let mut g = GitRunner::new(&format!("{}:{}", s(&bin), path_var()), &s(root.path()), "token");
    g.local_timeout = Duration::from_millis(200);

    let err = g
        .run(&s(root.path()), false, &["version"], &CancellationToken::new())
        .await
        .unwrap_err();

    assert_eq!(err.0, "git version: context deadline exceeded (signal: killed): ");
}

#[tokio::test]
async fn nothing_starts_once_heinzel_is_shutting_down() {
    let root = TempDir::new().unwrap();
    let marker = root.path().join("ran");
    let bin = root.path().join("bin");
    fs::create_dir(&bin).unwrap();
    fs::write(bin.join("git"), format!("#!/bin/sh\ntouch {}\n", marker.display())).unwrap();
    fs::set_permissions(bin.join("git"), fs::Permissions::from_mode(0o755)).unwrap();
    let g = GitRunner::new(&s(&bin), &s(root.path()), "token");
    let cancel = CancellationToken::new();
    cancel.cancel();

    let err = g.run(&s(root.path()), false, &["version"], &cancel).await.unwrap_err();

    assert_eq!(err.0, "git version: context canceled (context canceled): ");
    assert!(!marker.exists(), "git ran");
}

#[tokio::test]
async fn cancelling_kills_a_running_command() {
    let root = TempDir::new().unwrap();
    let bin = root.path().join("bin");
    fs::create_dir(&bin).unwrap();
    fs::write(bin.join("git"), "#!/bin/sh\nexec sleep 10\n").unwrap();
    fs::set_permissions(bin.join("git"), fs::Permissions::from_mode(0o755)).unwrap();
    let g = GitRunner::new(&format!("{}:{}", s(&bin), path_var()), &s(root.path()), "token");
    let cancel = CancellationToken::new();
    let dir = s(root.path());

    let (got, ()) = tokio::join!(g.run(&dir, false, &["version"], &cancel), async {
        tokio::time::sleep(Duration::from_millis(500)).await;
        cancel.cancel();
    });

    assert_eq!(got.unwrap_err().0, "git version: context canceled (signal: killed): ");
}

#[test]
fn network_commands_get_the_token_and_abort_slow_transfers() {
    let g = GitRunner::new("/usr/bin", "/w/run/git-home", "token");
    let pairs = |env: Vec<(String, String)>| -> Vec<String> {
        env.into_iter().map(|(k, v)| format!("{k}={v}")).collect()
    };

    let base = [
        "PATH=/usr/bin",
        "HOME=/w/run/git-home",
        "GIT_TERMINAL_PROMPT=0",
        "GIT_CONFIG_NOSYSTEM=1",
        "GIT_CONFIG_GLOBAL=/dev/null",
    ];
    assert_eq!(pairs(g.env(false)), base);
    let mut network: Vec<String> = base.iter().map(|s| (*s).to_owned()).collect();
    network.extend(
        [
            "GIT_CONFIG_COUNT=3",
            "GIT_CONFIG_KEY_0=http.extraHeader",
            &format!("GIT_CONFIG_VALUE_0=Authorization: Basic {}", BASE64.encode("x-access-token:token")),
            "GIT_CONFIG_KEY_1=http.lowSpeedLimit",
            "GIT_CONFIG_VALUE_1=1000",
            "GIT_CONFIG_KEY_2=http.lowSpeedTime",
            "GIT_CONFIG_VALUE_2=60",
        ]
        .map(String::from),
    );
    assert_eq!(pairs(g.env(true)), network);
}

#[tokio::test]
async fn the_git_runner_waits_only_briefly_for_pipes_a_child_still_holds() {
    // A fake git that leaves a child holding its output pipe, as a lingering
    // git-remote-https would, then hangs.
    let root = TempDir::new().unwrap();
    let bin = root.path().join("bin");
    fs::create_dir(&bin).unwrap();
    fs::write(bin.join("git"), "#!/bin/sh\n(sleep 10; echo late) &\nexec sleep 10\n").unwrap();
    fs::set_permissions(bin.join("git"), fs::Permissions::from_mode(0o755)).unwrap();
    let mut g = GitRunner::new(&format!("{}:{}", s(&bin), path_var()), &s(root.path()), "token");
    assert_eq!(g.wait_delay, Duration::from_secs(10));
    // Long enough for the script to start and fork.
    g.local_timeout = Duration::from_secs(1);
    g.wait_delay = Duration::from_millis(100);
    // The child's PATH finds the fake: Rust looks the program up in the
    // PATH it hands the child.

    let start = Instant::now();
    let err = g.run(&s(root.path()), false, &["version"], &CancellationToken::new()).await.unwrap_err();

    assert!(start.elapsed() < Duration::from_secs(5), "took {:?}", start.elapsed());
    assert!(err.0.starts_with("git version: context deadline exceeded ("), "{err}");
}

#[test]
fn make_removable_sets_dirs_to_owner_only_and_follows_no_symlinks() {
    let root = TempDir::new().unwrap();
    let run = root.path().join("run");
    let open = run.join("repo/open");
    fs::create_dir_all(&open).unwrap();
    fs::set_permissions(&open, fs::Permissions::from_mode(0o777)).unwrap();
    let outside = TempDir::new().unwrap();
    fs::set_permissions(outside.path(), fs::Permissions::from_mode(0o555)).unwrap();
    std::os::unix::fs::symlink(outside.path(), run.join("repo/link")).unwrap();

    make_removable(&run);

    for dir in [run.clone(), run.join("repo"), open] {
        assert_eq!(fs::symlink_metadata(&dir).unwrap().permissions().mode() & 0o777, 0o700, "{}", dir.display());
    }
    assert_eq!(fs::metadata(outside.path()).unwrap().permissions().mode() & 0o777, 0o555);
    fs::set_permissions(outside.path(), fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn remove_tree_removes_read_only_dirs() {
    let root = TempDir::new().unwrap();
    let run = root.path().join("run");
    let locked = run.join("repo/locked");
    fs::create_dir_all(&locked).unwrap();
    fs::write(locked.join("file"), "x").unwrap();
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o500)).unwrap();
    fs::set_permissions(run.join("repo"), fs::Permissions::from_mode(0o000)).unwrap();

    remove_tree(&s(&run)).unwrap();

    assert!(!run.exists());
}
