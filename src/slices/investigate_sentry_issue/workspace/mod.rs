//! The git side of an investigation run: a run dir with a mirror of the
//! GitHub repo and a clone for Claude to work in, and the git commands that
//! inspect Claude's commits and push them.

mod git;
mod manifest;
mod work_dir;

#[cfg(test)]
mod tests;

use std::fs;
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::path::Path;

use tokio_util::sync::CancellationToken;

use super::paths::clean;
use git::GitRunner;
use manifest::Manifest;
pub(crate) use work_dir::{check_dir, default_dir};

const GIT_USER_NAME: &str = "heinzel";
const GIT_USER_EMAIL: &str = "heinzel@users.noreply.github.com";
/// Where the mirror keeps the clone's branches for inspection.
const CLONE_REFS: &str = "refs/heinzel/clone/";
/// Every git command in the mirror runs with these, so hooks planted there
/// never run.
const HOOKS_OFF: [&str; 2] = ["-c", "core.hooksPath=/dev/null"];
/// Every git command that reaches another repo runs with these: local paths
/// and https only, whatever a repo's config says (a specific
/// `protocol.<name>.allow` in a config file would beat a general
/// `protocol.allow`, so each one is set).
const PROTOCOLS: [&str; 16] = [
    "-c", "protocol.allow=never",
    "-c", "protocol.file.allow=always",
    "-c", "protocol.https.allow=always",
    "-c", "protocol.http.allow=never",
    "-c", "protocol.ext.allow=never",
    "-c", "protocol.fd.allow=never",
    "-c", "protocol.ssh.allow=never",
    "-c", "protocol.git.allow=never",
];
/// Every fetch from the clone runs with these: no automatic gc or
/// maintenance afterwards, every object checked, no bundle URIs.
const FETCH_SAFETY: [&str; 8] = [
    "-c", "gc.auto=0",
    "-c", "maintenance.auto=false",
    "-c", "fetch.fsckObjects=true",
    "-c", "transfer.bundleURI=false",
];
/// Paths that would make git read another repo's config or objects in
/// place of the mirror's, or hide commits from heinzel's checks. None
/// exists in a fresh bare clone.
const FOREIGN_PATHS: [&str; 6] = [
    "commondir",
    ".git",
    "gitdir",
    "objects/info/alternates",
    "refs/replace",
    "shallow",
];

/// A workspace operation failed; the text says why.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{0}")]
pub(crate) struct Error(pub(crate) String);

/// The per-run directory layout, all under one temp run dir:
///
/// - `mirror`: a bare clone of the GitHub repo. Only heinzel uses it, with
///   the GitHub token, to clone from and to push from.
/// - `clone`: a clone of the mirror on the investigation branch, where Claude
///   works. It never holds the token or a GitHub remote.
/// - `home` and `claude_config`: the child's `HOME` and `CLAUDE_CONFIG_DIR`.
/// - `git_home`: the `HOME` of heinzel's own git commands. The child never
///   sees it and can't write to it, so it can't plant git config (such as
///   `url.<x>.insteadOf`) that heinzel's push with the token would honour.
///
/// Pushing from the mirror rather than the clone keeps Claude's edits to the
/// clone's `.git` (hooks, config, remotes) away from the token: heinzel only
/// fetches the branch's commits out of the clone. The mirror is guarded too,
/// in case Claude reaches it anyway:
///
/// - its dirs are read-only while Claude runs;
/// - heinzel's git commands in it pin `GIT_DIR` and `GIT_COMMON_DIR` to it,
///   ignore replace refs, run no hooks and speak only to local paths and
///   https;
/// - the push names the repo's URL rather than a remote;
/// - any change to the mirror while Claude ran, and anything at push time
///   that would make git read another repo, stops the run before heinzel
///   fetches into the mirror or pushes from it.
#[derive(Debug, Clone, Default)]
pub(crate) struct Workspace {
    /// The work dir, symlinks resolved.
    pub(crate) work_dir: String,
    /// The work dir as configured (cleaned), which may differ from
    /// `work_dir` by symlinks.
    pub(crate) given_work_dir: String,
    /// The other entries in the work dir when the run began, such as
    /// leftovers of earlier runs, sorted.
    pub(crate) siblings: Vec<String>,
    pub(crate) run_dir: String,
    pub(crate) mirror: String,
    pub(crate) clone: String,
    pub(crate) home: String,
    pub(crate) claude_config: String,
    pub(crate) git_home: String,
    pub(crate) branch: String,
    pub(crate) default_branch: String,
    /// The GitHub repo's URL, which the push names.
    remote_url: String,
    /// The mirror's config file as heinzel left it.
    mirror_config: Vec<u8>,
    /// Every entry in the mirror as heinzel left it.
    mirror_manifest: Manifest,
    /// The branch's commit whose changes [`Workspace::changed_paths`]
    /// listed, which is what the push sends.
    checked_tip: Option<String>,
    /// Every entry in the mirror right after
    /// [`Workspace::changed_paths`]; the push requires it unchanged.
    checked_manifest: Manifest,
    /// The default branch's commit the branch starts from.
    base: String,
    /// Set by [`Workspace::stray_branches`] when the clone no longer has the
    /// branch.
    branch_gone: bool,
    git: GitRunner,
}

impl Workspace {
    /// Creates a run dir under `work_dir`, mirrors `clone_url` into it and
    /// checks out `branch` from the default branch in a clone of the mirror.
    /// Git runs with `path` as its `PATH`, and with `token` for the commands
    /// that talk to GitHub. On error it removes what it created.
    pub(crate) async fn new(
        work_dir: &str,
        clone_url: &str,
        branch: &str,
        path: &str,
        token: &str,
        cancel: &CancellationToken,
    ) -> Result<Workspace, Error> {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(work_dir)
            .map_err(|err| Error(format!("create work dir: {err}")))?;
        check_dir(work_dir)?;
        let created = tempfile::Builder::new()
            .prefix("heinzel-investigation-")
            .tempdir_in(work_dir)
            .map_err(|err| Error(format!("create run dir: {err}")))?
            .keep();
        let created = created.to_string_lossy().into_owned();
        match Workspace::set_up(&created, clone_url, branch, path, token, cancel).await {
            Ok(mut ws) => {
                ws.given_work_dir = clean(work_dir);
                Ok(ws)
            }
            Err(err) => {
                let _ = remove_dir(created).await;
                Err(err)
            }
        }
    }

    async fn set_up(
        created: &str,
        clone_url: &str,
        branch: &str,
        path: &str,
        token: &str,
        cancel: &CancellationToken,
    ) -> Result<Workspace, Error> {
        // Resolve symlinks (e.g. macOS /var -> /private/var) so the sandbox's
        // write allowlist names the path the child actually sees.
        let run_dir = fs::canonicalize(created)
            .map_err(|err| Error(err.to_string()))?
            .to_string_lossy()
            .into_owned();
        let (work_dir, run_name) = run_dir
            .rsplit_once('/')
            .ok_or_else(|| Error(format!("run dir {run_dir} has no parent")))?;
        let mut siblings: Vec<String> = fs::read_dir(work_dir)
            .map_err(|err| Error(err.to_string()))?
            .filter_map(Result::ok)
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name != run_name)
            .collect();
        siblings.sort();
        let join = |name: &str| format!("{run_dir}/{name}");
        let mut ws = Workspace {
            work_dir: if work_dir.is_empty() { "/".to_owned() } else { work_dir.to_owned() },
            siblings: siblings.iter().map(|s| format!("{work_dir}/{s}")).collect(),
            mirror: join("mirror.git"),
            clone: join("repo"),
            home: join("home"),
            claude_config: join("claude-config"),
            git_home: join("git-home"),
            branch: branch.to_owned(),
            remote_url: clone_url.to_owned(),
            git: GitRunner::new(path, &join("git-home"), token),
            run_dir: run_dir.clone(),
            ..Workspace::default()
        };
        for dir in [&ws.home, &ws.claude_config, &ws.git_home] {
            fs::DirBuilder::new()
                .mode(0o700)
                .create(dir)
                .map_err(|err| Error(err.to_string()))?;
        }
        let mut clone_args = PROTOCOLS.to_vec();
        clone_args.extend(["clone", "--quiet", "--bare", clone_url, &ws.mirror]);
        ws.git.run(&run_dir, true, &clone_args, cancel).await?;
        ws.default_branch = ws.in_mirror(false, &["symbolic-ref", "--short", "HEAD"], cancel).await?;
        ws.base = ws
            .in_mirror(false, &["rev-parse", "--verify", "HEAD^{commit}"], cancel)
            .await?;
        // --no-hardlinks: Claude may chmod and rewrite the clone's object
        // files, which must not change the mirror's.
        let mut clone_args = PROTOCOLS.to_vec();
        clone_args.extend(["clone", "--quiet", "--no-hardlinks", &ws.mirror, &ws.clone]);
        ws.git.run(&run_dir, false, &clone_args, cancel).await?;
        for args in [
            &["config", "user.name", GIT_USER_NAME][..],
            &["config", "user.email", GIT_USER_EMAIL],
            &["config", "commit.gpgsign", "false"],
            &["checkout", "--quiet", "-b", branch],
        ] {
            ws.git.run(&ws.clone, false, args, cancel).await?;
        }
        ws.mirror_config = ws.read_mirror_config()?;
        ws.mirror_manifest = ws.take_mirror_manifest()?;
        Ok(ws)
    }

    /// Runs git on the mirror: `GIT_DIR` and `GIT_COMMON_DIR` pinned to it
    /// (so neither a `.git` file nor a `commondir` in it can point git at
    /// another repo), replace refs ignored, hooks off and only local paths
    /// and https allowed.
    async fn in_mirror(
        &self,
        auth: bool,
        args: &[&str],
        cancel: &CancellationToken,
    ) -> Result<String, Error> {
        let mut all = HOOKS_OFF.to_vec();
        all.extend(["-c", "core.commitGraph=false"]);
        all.extend(PROTOCOLS);
        all.extend_from_slice(args);
        let env = [
            ("GIT_DIR", self.mirror.as_str()),
            ("GIT_COMMON_DIR", self.mirror.as_str()),
            ("GIT_NO_REPLACE_OBJECTS", "1"),
        ];
        self.git.run_with_env(&self.mirror, auth, &all, &env, cancel).await
    }

    fn read_mirror_config(&self) -> Result<Vec<u8>, Error> {
        fs::read(format!("{}/config", self.mirror))
            .map_err(|err| Error(format!("read the mirror's git config: {err}")))
    }

    fn take_mirror_manifest(&self) -> Result<Manifest, Error> {
        Manifest::take(Path::new(&self.mirror))
            .map_err(|err| Error(format!("record the mirror: {err}")))
    }

    /// Makes the mirror's dirs read-only (0500) while Claude runs, so
    /// nothing can be added, removed or renamed in it.
    /// [`Workspace::unlock_mirror`] undoes it; removing the workspace works
    /// either way.
    pub(crate) fn lock_mirror(&self) -> Result<(), Error> {
        for (dir, _) in self.mirror_manifest.dirs() {
            fs::set_permissions(self.mirror_path(dir), fs::Permissions::from_mode(0o500))
                .map_err(|err| Error(format!("lock the mirror: {err}")))?;
        }
        Ok(())
    }

    /// Gives the mirror's dirs back the modes they had. A dir it can't
    /// restore makes [`Workspace::check_mirror`] fail.
    pub(crate) fn unlock_mirror(&self) {
        for (dir, mode) in self.mirror_manifest.dirs() {
            let _ = fs::set_permissions(self.mirror_path(dir), fs::Permissions::from_mode(mode));
        }
    }

    fn mirror_path(&self, rel: &str) -> String {
        if rel.is_empty() {
            self.mirror.clone()
        } else {
            format!("{}/{rel}", self.mirror)
        }
    }

    /// Fails if anything in the mirror differs from what heinzel left:
    /// any entry added, removed or changed in type, mode, target or content.
    /// Claude can't reach the mirror through its sandbox, but if it did, a
    /// changed mirror could send the push, with the token, elsewhere, or run
    /// code outside the sandbox when heinzel fetches into it.
    pub(crate) fn check_mirror(&self) -> Result<(), Error> {
        match self.mirror_manifest.first_difference(&self.take_mirror_manifest()?) {
            None => Ok(()),
            Some(path) => Err(Error(format!(
                "the mirror changed while claude ran ({}), so heinzel won't push from it",
                if path.is_empty() { "." } else { &path }
            ))),
        }
    }

    /// Fails unless the mirror is fit to push from: unchanged since
    /// [`Workspace::changed_paths`], its config still the regular file
    /// heinzel wrote, nothing pointing git at another repo, and no symlinks.
    fn check_push(&self) -> Result<(), Error> {
        let refuse = |what: String| Error(format!("{what}, so heinzel won't push from it"));
        let now = self.take_mirror_manifest()?;
        if let Some(path) = self.checked_manifest.first_difference(&now) {
            return Err(refuse(format!("the mirror changed after heinzel checked it ({path})")));
        }
        let config = format!("{}/config", self.mirror);
        let regular = Path::new(&config).symlink_metadata().is_ok_and(|m| m.is_file());
        if !regular || self.read_mirror_config()? != self.mirror_config {
            return Err(refuse("the mirror's git config changed".to_owned()));
        }
        for name in FOREIGN_PATHS {
            let missing = Path::new(&self.mirror_path(name))
                .symlink_metadata()
                .is_err_and(|err| err.kind() == std::io::ErrorKind::NotFound);
            if !missing {
                return Err(refuse(format!("the mirror holds {name}")));
            }
        }
        if let Some(link) = now.first_symlink() {
            return Err(refuse(format!("the mirror holds a symlink at {link}")));
        }
        Ok(())
    }

    /// Deletes the run dir. One that is already gone counts as deleted.
    pub(crate) async fn remove(&self) -> std::io::Result<()> {
        remove_dir(self.run_dir.clone()).await
    }

    /// Returns the clone's branches other than the investigation branch that
    /// hold commits neither the mirror nor the investigation branch has: work
    /// Claude committed somewhere heinzel won't push. It also notes whether
    /// the clone still has the investigation branch, which Claude may have
    /// renamed or deleted.
    pub(crate) async fn stray_branches(
        &mut self,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>, Error> {
        let refspec = format!("+refs/heads/*:{CLONE_REFS}*");
        self.in_mirror(
            false,
            &[
                &FETCH_SAFETY[..],
                &["fetch", "--quiet", "--no-tags", "--no-recurse-submodules", &self.clone, &refspec],
            ]
            .concat(),
            cancel,
        )
        .await?;
        let out = self
            .in_mirror(false, &["for-each-ref", "--format=%(refname)", CLONE_REFS], cancel)
            .await?;
        let refs: Vec<&str> = out.lines().filter(|r| !r.is_empty()).collect();
        let own = format!("{CLONE_REFS}{}", self.branch);
        self.branch_gone = !refs.contains(&own.as_str());
        let mut not = vec!["--not", "--branches"];
        if !self.branch_gone {
            not.push(&own);
        }
        let mut stray = Vec::new();
        for r in refs {
            let name = r.strip_prefix(CLONE_REFS).unwrap_or(r);
            if name == self.branch {
                continue;
            }
            // Commits that also reached the investigation branch are pushed
            // with it, so they don't count.
            let mut args = vec!["rev-list", "--count", r];
            args.extend(&not);
            if self.in_mirror(false, &args, cancel).await? != "0" {
                stray.push(name.to_owned());
            }
        }
        Ok(stray)
    }

    /// Fetches the branch from the clone into the mirror and counts its
    /// commits on top of the base. It runs after
    /// [`Workspace::stray_branches`]; a branch that is gone from the clone
    /// has none.
    pub(crate) async fn new_commits(&self, cancel: &CancellationToken) -> Result<u64, Error> {
        if self.branch_gone {
            return Ok(0);
        }
        let r = format!("refs/heads/{}", self.branch);
        let refspec = format!("+{r}:{r}");
        self.in_mirror(
            false,
            &[
                &FETCH_SAFETY[..],
                &["fetch", "--quiet", "--no-tags", "--no-recurse-submodules", &self.clone, &refspec],
            ]
            .concat(),
            cancel,
        )
        .await?;
        let range = format!("{}..{r}", self.base);
        let out = self.in_mirror(false, &["rev-list", "--count", &range], cancel).await?;
        out.parse()
            .map_err(|_| Error(format!("count commits: {out:?} is not a number")))
    }

    /// Lists the paths the branch changes on top of the base, renames listed
    /// as a deletion and an addition, submodules included. It notes the
    /// branch's commit it looked at, which is what the push sends, and the
    /// mirror as it is then, which the push requires unchanged. It runs
    /// after [`Workspace::new_commits`].
    pub(crate) async fn changed_paths(
        &mut self,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>, Error> {
        let r = format!("refs/heads/{}^{{commit}}", self.branch);
        let tip = self.in_mirror(false, &["rev-parse", "--verify", &r], cancel).await?;
        let out = self
            .in_mirror(
                false,
                &[
                    "diff",
                    "--name-only",
                    "--no-renames",
                    "--ignore-submodules=none",
                    "-z",
                    &self.base,
                    &tip,
                ],
                cancel,
            )
            .await?;
        self.checked_tip = Some(tip);
        self.checked_manifest = self.take_mirror_manifest()?;
        Ok(out
            .split('\0')
            .filter(|p| !p.is_empty())
            .map(str::to_owned)
            .collect())
    }

    /// Pushes the commit [`Workspace::changed_paths`] checked to the
    /// branch at the GitHub repo's URL, after checking the mirror is fit to
    /// push from.
    pub(crate) async fn push(&self, cancel: &CancellationToken) -> Result<(), Error> {
        let Some(tip) = &self.checked_tip else {
            return Err(Error(
                "heinzel hasn't checked the branch's changes, so it won't push it".to_owned(),
            ));
        };
        self.check_push()?;
        let refspec = format!("{tip}:refs/heads/{}", self.branch);
        self.in_mirror(
            true,
            &["push", "--quiet", "--no-verify", &self.remote_url, &refspec],
            cancel,
        )
        .await
        .map(drop)
    }
}

/// Deletes `dir` off the async threads, counting one that is already gone
/// as deleted. A failed or panicked removal is an I/O error.
pub(crate) async fn remove_dir(dir: String) -> std::io::Result<()> {
    tokio::task::spawn_blocking(move || remove_tree(&dir))
        .await
        .map_err(std::io::Error::other)?
}

/// Deletes `dir` and everything in it. It first makes every directory in the
/// tree removable, so that read-only directories (git's or the child's)
/// don't stop the removal. A missing `dir` counts as deleted.
fn remove_tree(dir: &str) -> std::io::Result<()> {
    make_removable(Path::new(dir));
    match fs::remove_dir_all(dir) {
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        result => result,
    }
}

/// Sets every directory in the tree to 0700, each before reading it. It
/// doesn't follow symlinks, and the mode is fixed rather than derived from
/// the entry, so nothing outside the tree gains permissions.
fn make_removable(dir: &Path) {
    if !dir.symlink_metadata().is_ok_and(|m| m.is_dir()) {
        return;
    }
    let _ = fs::set_permissions(dir, fs::Permissions::from_mode(0o700));
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.filter_map(Result::ok) {
        if entry.file_type().is_ok_and(|t| t.is_dir()) {
            make_removable(&entry.path());
        }
    }
}
