//! The slice's settings, loaded from the environment.

use std::collections::HashMap;
use std::fmt;
use std::time::Duration;

use super::duration::parse_go_duration;
use super::paths::{clean, first_containing, look_path, resolve_path, with_resolved, within};

const REPOS_VAR: &str = "SENTRY_GITHUB_REPOS";
const GITHUB_TOKEN_VAR: &str = "GITHUB_TOKEN";
const ANTHROPIC_KEY_VAR: &str = "ANTHROPIC_API_KEY";
const OPENAI_KEY_VAR: &str = "OPENAI_API_KEY";
const PLUGIN_DIRS_VAR: &str = "CLAUDE_PLUGIN_DIRS";
const TIMEOUT_VAR: &str = "INVESTIGATION_TIMEOUT";
const MAX_AGE_VAR: &str = "INVESTIGATION_MAX_AGE";
const CLAUDE_BIN_VAR: &str = "CLAUDE_BIN";
pub(super) const WORK_DIR_VAR: &str = "INVESTIGATION_WORK_DIR";
const DENY_READ_VAR: &str = "INVESTIGATION_DENY_READ";

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30 * 60);
const DEFAULT_MAX_AGE: Duration = Duration::from_secs(60 * 60);
const DEFAULT_CLAUDE_BIN: &str = "claude";

/// A GitHub repository.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct Repo {
    pub(super) owner: String,
    pub(super) name: String,
}

impl fmt::Display for Repo {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}/{}", self.owner, self.name)
    }
}

/// The slice's settings.
#[derive(Clone, PartialEq, Eq)]
pub(super) struct Config {
    /// By Sentry project id.
    pub(super) repos: HashMap<i64, Repo>,
    pub(super) github_token: String,
    pub(super) anthropic_key: String,
    pub(super) openai_key: String,
    pub(super) plugin_dirs: Vec<String>,
    pub(super) timeout: Duration,
    pub(super) max_age: Duration,
    pub(super) claude_bin: String,
    /// Empty means the workspace's default dir.
    pub(super) work_dir: String,
    /// The `PATH` handed to the claude child and to git.
    pub(super) path: String,
    /// heinzel's own `HOME`; empty if unset. Neither the child's file tools
    /// nor its sandboxed commands may read it.
    pub(super) parent_home: String,
    /// More paths the child may not read, such as heinzel's data dir.
    pub(super) deny_read: Vec<String>,
    /// The paths under denied ones that sandboxed commands still need to
    /// read: the plugin dirs and the claude binary's dir. Set by
    /// [`Config::resolve_read_paths`].
    pub(super) readable: Vec<String>,
}

impl fmt::Debug for Config {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Config")
            .field("repos", &self.repos)
            .field("github_token", &"[redacted]")
            .field("anthropic_key", &"[redacted]")
            .field("openai_key", &"[redacted]")
            .field("plugin_dirs", &self.plugin_dirs)
            .field("timeout", &self.timeout)
            .field("max_age", &self.max_age)
            .field("claude_bin", &self.claude_bin)
            .field("work_dir", &self.work_dir)
            .field("path", &self.path)
            .field("parent_home", &self.parent_home)
            .field("deny_read", &self.deny_read)
            .field("readable", &self.readable)
            .finish()
    }
}

/// Every invalid setting, one message each, naming its variable.
#[derive(Debug)]
pub(super) struct InvalidConfig(Vec<String>);

impl fmt::Display for InvalidConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0.join("\n"))
    }
}

impl std::error::Error for InvalidConfig {}

impl Config {
    /// Reads the settings through `getenv`. Any invalid value is reported,
    /// naming its variable.
    pub(super) fn load(getenv: &dyn Fn(&str) -> Option<String>) -> Result<Config, InvalidConfig> {
        let var = |name: &str| getenv(name).unwrap_or_default();
        let mut errors = Vec::new();
        let repos = check(&mut errors, REPOS_VAR, parse_repos(&var(REPOS_VAR)));
        let plugin_dirs = check(
            &mut errors,
            PLUGIN_DIRS_VAR,
            parse_list(&var(PLUGIN_DIRS_VAR)).and_then(absolute),
        );
        let timeout = check(&mut errors, TIMEOUT_VAR, parse_duration(&var(TIMEOUT_VAR), DEFAULT_TIMEOUT));
        let max_age = check(&mut errors, MAX_AGE_VAR, parse_duration(&var(MAX_AGE_VAR), DEFAULT_MAX_AGE));
        let deny_read = check(&mut errors, DENY_READ_VAR, parse_paths(&var(DENY_READ_VAR)));
        let work_dir = var(WORK_DIR_VAR);
        if !work_dir.is_empty() && !work_dir.starts_with('/') {
            errors.push(format!("{WORK_DIR_VAR}: {work_dir:?} is not an absolute path"));
        }
        for name in [GITHUB_TOKEN_VAR, ANTHROPIC_KEY_VAR, OPENAI_KEY_VAR] {
            if var(name).is_empty() {
                errors.push(format!("{name} must be set"));
            }
        }
        if !errors.is_empty() {
            return Err(InvalidConfig(errors));
        }
        let home = var("HOME");
        let claude_bin = var(CLAUDE_BIN_VAR);
        Ok(Config {
            repos,
            github_token: var(GITHUB_TOKEN_VAR),
            anthropic_key: var(ANTHROPIC_KEY_VAR),
            openai_key: var(OPENAI_KEY_VAR),
            plugin_dirs,
            timeout,
            max_age,
            claude_bin: if claude_bin.is_empty() {
                DEFAULT_CLAUDE_BIN.to_owned()
            } else {
                claude_bin
            },
            work_dir,
            path: var("PATH"),
            parent_home: if home.is_empty() { home } else { clean(&home) },
            deny_read,
            readable: Vec::new(),
        })
    }

    /// Checks that the paths the child must read stay readable despite its
    /// read denials, and sets `readable`. `work_dir` must be set.
    ///
    /// Claude's file tools are denied heinzel's HOME and the deny-read paths
    /// with Read deny rules, which nothing can carve an exception out of, so
    /// the work dir and the plugin dirs must lie outside them. Sandboxed
    /// commands are also denied the work dir; a narrower allowRead re-opens a
    /// path inside a denied one, but never a denied path inside it. So no
    /// denied path may lie inside a plugin dir or the claude binary's dir.
    pub(super) fn resolve_read_paths(&mut self) -> Result<(), String> {
        let file_denied: Vec<String> = std::iter::once(&self.parent_home)
            .chain(&self.deny_read)
            .filter(|p| !p.is_empty())
            .map(|p| resolve_path(p))
            .collect();
        let work_dir = resolve_path(&self.work_dir);
        let mut sandbox_denied = file_denied.clone();
        sandbox_denied.push(work_dir.clone());

        if let Some(denied) = first_containing(&file_denied, &work_dir) {
            return Err(format!(
                "{WORK_DIR_VAR}: {} lies under {denied}, which the child may not read (HOME or {DENY_READ_VAR})",
                self.work_dir
            ));
        }
        let mut readable = Vec::new();
        for dir in &self.plugin_dirs {
            let p = resolve_path(dir);
            if let Some(d) = sandbox_denied.iter().find(|d| within(&p, d) || within(d, &p)) {
                return Err(format!(
                    "{PLUGIN_DIRS_VAR}: plugin dir {dir} overlaps {d}, which the child may not read (HOME, {DENY_READ_VAR} or {WORK_DIR_VAR}); move the plugins elsewhere"
                ));
            }
            readable.push(p);
        }
        if let Some(bin) = look_path(&self.claude_bin, &self.path) {
            let bin_dir = parent(&resolve_path(&bin));
            if let Some(d) = sandbox_denied.iter().find(|d| within(d, &bin_dir)) {
                return Err(format!(
                    "{CLAUDE_BIN_VAR}: {} lies in {bin_dir}, which holds {d}, a path the child may not read",
                    self.claude_bin
                ));
            }
            readable.push(bin_dir);
        }
        self.readable = readable;
        Ok(())
    }

    /// Returns the `PATH` entries that lie, as given or with symlinks
    /// resolved, under HOME, a deny-read path or the work dir, and that no
    /// readable path re-opens, each with the denied path it lies under.
    /// Sandboxed commands can't run tools from them. `work_dir` and
    /// `readable` must be set.
    pub(super) fn denied_path_dirs(&self) -> Vec<(String, String)> {
        let mut denied = vec![self.parent_home.clone(), self.work_dir.clone()];
        denied.extend(self.deny_read.iter().cloned());
        let denied = with_resolved(&denied);
        let mut found = Vec::new();
        for dir in self.path.split(':').filter(|dir| !dir.is_empty()) {
            let forms = with_resolved(&[clean(dir)]);
            if forms
                .iter()
                .any(|f| first_containing(&self.readable, f).is_some())
            {
                continue;
            }
            if let Some(d) = forms.iter().find_map(|f| first_containing(&denied, f)) {
                found.push((dir.to_owned(), d.to_owned()));
            }
        }
        found
    }
}

/// Returns the parsed value, or notes the error, naming `name`, and
/// returns a default.
fn check<T: Default>(errors: &mut Vec<String>, name: &str, result: Result<T, String>) -> T {
    result.unwrap_or_else(|err| {
        errors.push(format!("{name}: {err}"));
        T::default()
    })
}

/// The dir holding `path`, like Go's `filepath.Dir` for a clean absolute
/// path.
fn parent(path: &str) -> String {
    match path.rsplit_once('/') {
        Some(("", _)) => "/".to_owned(),
        Some((dir, _)) => dir.to_owned(),
        None => ".".to_owned(),
    }
}

/// Parses comma-separated `<projectId>=<owner>/<repo>` pairs.
fn parse_repos(value: &str) -> Result<HashMap<i64, Repo>, String> {
    let mut repos = HashMap::new();
    for pair in parse_list(value)? {
        let Some((id_text, name)) = pair.split_once('=') else {
            return Err(format!("{pair:?} is not <projectId>=<owner>/<repo>"));
        };
        let id: i64 = id_text
            .trim()
            .parse()
            .map_err(|_| format!("project id {id_text:?} is not an integer"))?;
        if repos.contains_key(&id) {
            return Err(format!("project id {id} is mapped twice"));
        }
        let repo = name
            .trim()
            .split_once('/')
            .filter(|(owner, repo)| valid_repo_part(owner) && valid_repo_part(repo))
            .ok_or_else(|| format!("{name:?} is not a GitHub <owner>/<repo>"))?;
        repos.insert(
            id,
            Repo {
                owner: repo.0.to_owned(),
                name: repo.1.to_owned(),
            },
        );
    }
    Ok(repos)
}

/// One segment of a GitHub owner/repo name: `[A-Za-z0-9_.-]+`, but not `.`
/// or `..`.
fn valid_repo_part(s: &str) -> bool {
    !s.is_empty()
        && s
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
        && s != "."
        && s != ".."
}

/// Splits a comma-separated list, trimming each entry. It rejects an empty
/// list and empty entries.
fn parse_list(value: &str) -> Result<Vec<String>, String> {
    if value.trim().is_empty() {
        return Err("must be set".to_owned());
    }
    value
        .split(',')
        .map(|item| match item.trim() {
            "" => Err(format!("{value:?} has an empty entry")),
            item => Ok(item.to_owned()),
        })
        .collect()
}

/// Parses an optional comma-separated list of absolute paths, cleaned.
fn parse_paths(value: &str) -> Result<Vec<String>, String> {
    if value.trim().is_empty() {
        return Ok(Vec::new());
    }
    let paths = absolute(parse_list(value)?)?;
    Ok(paths.iter().map(|p| clean(p)).collect())
}

/// Fails unless every one of `paths` is absolute.
fn absolute(paths: Vec<String>) -> Result<Vec<String>, String> {
    match paths.iter().find(|p| !p.starts_with('/')) {
        Some(relative) => Err(format!("{relative:?} is not an absolute path")),
        None => Ok(paths),
    }
}

/// Parses a positive Go duration such as `30m` or `1h30m`, or returns
/// `default` for "".
fn parse_duration(value: &str, default: Duration) -> Result<Duration, String> {
    if value.is_empty() {
        return Ok(default);
    }
    let nanos = parse_go_duration(value)?;
    match u64::try_from(nanos) {
        Ok(nanos) if nanos > 0 => Ok(Duration::from_nanos(nanos)),
        _ => Err(format!("{value} is not positive")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every required variable with a valid value, then `overrides`.
    fn with(overrides: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let mut env: HashMap<String, String> = [
            ("SENTRY_GITHUB_REPOS", "1=acme/web"),
            ("GITHUB_TOKEN", "gh-token"),
            ("ANTHROPIC_API_KEY", "anthropic-key"),
            ("OPENAI_API_KEY", "openai-key"),
            ("CLAUDE_PLUGIN_DIRS", "/plugins/codex"),
            ("PATH", "/usr/bin:/bin"),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_owned(), v.to_owned()))
        .collect();
        for (k, v) in overrides {
            env.insert((*k).to_owned(), (*v).to_owned());
        }
        move |key| env.get(key).cloned()
    }

    fn repo(owner: &str, name: &str) -> Repo {
        Repo {
            owner: owner.into(),
            name: name.into(),
        }
    }

    #[test]
    fn defaults() {
        let got = Config::load(&with(&[])).unwrap();

        let want = Config {
            repos: HashMap::from([(1, repo("acme", "web"))]),
            github_token: "gh-token".into(),
            anthropic_key: "anthropic-key".into(),
            openai_key: "openai-key".into(),
            plugin_dirs: vec!["/plugins/codex".into()],
            timeout: Duration::from_secs(30 * 60),
            max_age: Duration::from_secs(60 * 60),
            claude_bin: "claude".into(),
            work_dir: String::new(),
            path: "/usr/bin:/bin".into(),
            parent_home: String::new(),
            deny_read: Vec::new(),
            readable: Vec::new(),
        };
        assert_eq!(got, want);
    }

    #[test]
    fn debug_output_hides_the_secrets() {
        let debug = format!("{:?}", Config::load(&with(&[])).unwrap());

        for secret in ["gh-token", "anthropic-key", "openai-key"] {
            assert!(!debug.contains(secret), "{debug}");
        }
    }

    #[test]
    fn every_variable_is_read() {
        let got = Config::load(&with(&[
            ("SENTRY_GITHUB_REPOS", " 1=acme/web , 42=Other-Org/api.v2_x "),
            ("CLAUDE_PLUGIN_DIRS", "/p/codex, /p/auto-task"),
            ("INVESTIGATION_TIMEOUT", "90s"),
            ("INVESTIGATION_MAX_AGE", "15m"),
            ("CLAUDE_BIN", "/opt/claude"),
            ("INVESTIGATION_WORK_DIR", "/var/heinzel"),
            ("INVESTIGATION_DENY_READ", " /srv/data , /etc/heinzel/ "),
            ("HOME", "/home/heinzel/"),
        ]))
        .unwrap();

        assert_eq!(
            got.repos,
            HashMap::from([(1, repo("acme", "web")), (42, repo("Other-Org", "api.v2_x"))])
        );
        assert_eq!(got.plugin_dirs, ["/p/codex", "/p/auto-task"]);
        assert_eq!(got.deny_read, ["/srv/data", "/etc/heinzel"]);
        assert_eq!(got.parent_home, "/home/heinzel");
        assert_eq!(got.timeout, Duration::from_secs(90));
        assert_eq!(got.max_age, Duration::from_secs(15 * 60));
        assert_eq!(got.claude_bin, "/opt/claude");
        assert_eq!(got.work_dir, "/var/heinzel");
    }

    #[test]
    fn go_durations_are_accepted() {
        let got = Config::load(&with(&[("INVESTIGATION_TIMEOUT", "1.5h"), ("INVESTIGATION_MAX_AGE", "1m30s")])).unwrap();

        assert_eq!(got.timeout, Duration::from_secs(5400));
        assert_eq!(got.max_age, Duration::from_secs(90));
    }

    #[test]
    fn duration_errors_read_like_go_s() {
        let cases = [
            ("soon", r#"INVESTIGATION_TIMEOUT: time: invalid duration "soon""#),
            ("30", r#"INVESTIGATION_TIMEOUT: time: missing unit in duration "30""#),
            ("1d", r#"INVESTIGATION_TIMEOUT: time: unknown unit "d" in duration "1d""#),
            ("-1m", "INVESTIGATION_TIMEOUT: -1m is not positive"),
            ("0", "INVESTIGATION_TIMEOUT: 0 is not positive"),
        ];
        for (value, want) in cases {
            let err = Config::load(&with(&[("INVESTIGATION_TIMEOUT", value)])).unwrap_err();

            assert_eq!(err.to_string(), want);
        }
    }

    #[test]
    fn invalid_values_are_rejected_naming_their_variable() {
        let cases = [
            ("SENTRY_GITHUB_REPOS", ""),
            ("SENTRY_GITHUB_REPOS", "1"),
            ("SENTRY_GITHUB_REPOS", "x=acme/web"),
            ("SENTRY_GITHUB_REPOS", "1.5=acme/web"),
            ("SENTRY_GITHUB_REPOS", "1=acme"),
            ("SENTRY_GITHUB_REPOS", "1=acme/web/extra"),
            ("SENTRY_GITHUB_REPOS", "1=/web"),
            ("SENTRY_GITHUB_REPOS", "1=acme/"),
            ("SENTRY_GITHUB_REPOS", "1=acme/we b"),
            ("SENTRY_GITHUB_REPOS", "1=../web"),
            ("SENTRY_GITHUB_REPOS", "1=acme/.."),
            ("SENTRY_GITHUB_REPOS", "1=acme/web,"),
            ("SENTRY_GITHUB_REPOS", "1=acme/web,1=acme/api"),
            ("GITHUB_TOKEN", ""),
            ("ANTHROPIC_API_KEY", ""),
            ("OPENAI_API_KEY", ""),
            ("CLAUDE_PLUGIN_DIRS", ""),
            ("CLAUDE_PLUGIN_DIRS", "/p/codex,,/p/at"),
            ("INVESTIGATION_TIMEOUT", "30"),
            ("INVESTIGATION_TIMEOUT", "0s"),
            ("INVESTIGATION_TIMEOUT", "-1m"),
            ("INVESTIGATION_MAX_AGE", "soon"),
            ("INVESTIGATION_MAX_AGE", "0"),
            ("INVESTIGATION_DENY_READ", "relative/dir"),
            ("INVESTIGATION_DENY_READ", "/a,,/b"),
            ("INVESTIGATION_TIMEOUT", "1 h"),
            ("INVESTIGATION_TIMEOUT", "1day"),
            ("INVESTIGATION_WORK_DIR", "relative/work"),
            ("CLAUDE_PLUGIN_DIRS", "/p/codex,plugins/at"),
        ];
        for (variable, value) in cases {
            let err = Config::load(&with(&[(variable, value)])).unwrap_err().to_string();

            assert!(err.contains(variable), "{variable}={value:?}: got {err:?}");
        }
    }

    #[test]
    fn every_invalid_value_is_reported() {
        let err = Config::load(&with(&[("GITHUB_TOKEN", ""), ("INVESTIGATION_TIMEOUT", "0s")]))
            .unwrap_err()
            .to_string();

        assert_eq!(err, "INVESTIGATION_TIMEOUT: 0s is not positive\nGITHUB_TOKEN must be set");
    }

    #[test]
    fn repo_errors_read_like_go_s() {
        let cases = [
            ("1", r#"SENTRY_GITHUB_REPOS: "1" is not <projectId>=<owner>/<repo>"#),
            ("x=acme/web", r#"SENTRY_GITHUB_REPOS: project id "x" is not an integer"#),
            ("1=acme/web,1=a/b", "SENTRY_GITHUB_REPOS: project id 1 is mapped twice"),
            ("1=acme", r#"SENTRY_GITHUB_REPOS: "acme" is not a GitHub <owner>/<repo>"#),
            ("", "SENTRY_GITHUB_REPOS: must be set"),
        ];
        for (value, want) in cases {
            let err = Config::load(&with(&[("SENTRY_GITHUB_REPOS", value)])).unwrap_err();

            assert_eq!(err.to_string(), want);
        }
    }

    /// A config whose paths don't exist, so resolving them only cleans them.
    fn read_paths_config() -> Config {
        let mut cfg = Config::load(&with(&[])).unwrap();
        cfg.parent_home = "/home/heinzel".into();
        cfg.deny_read = vec!["/srv/data".into()];
        cfg.work_dir = "/var/heinzel".into();
        cfg.plugin_dirs = vec!["/opt/plugins/codex".into(), "/opt/plugins/auto-task/".into()];
        cfg.claude_bin = "/home/heinzel/.local/bin/claude".into();
        cfg
    }

    #[test]
    fn plugins_and_the_claude_binary_stay_readable() {
        let mut cfg = read_paths_config();

        cfg.resolve_read_paths().unwrap();

        assert_eq!(
            cfg.readable,
            ["/opt/plugins/codex", "/opt/plugins/auto-task", "/home/heinzel/.local/bin"]
        );
    }

    #[test]
    fn paths_the_sandbox_cannot_reopen_are_rejected() {
        type Change = fn(&mut Config);
        let cases: [(&str, Change, &str); 8] = [
            ("plugin dir under HOME", |c| c.plugin_dirs[0] = "/home/heinzel/.claude/plugins/codex".into(), "CLAUDE_PLUGIN_DIRS"),
            ("plugin dir under a denied path", |c| c.plugin_dirs[1] = "/srv/data/plugins/at".into(), "CLAUDE_PLUGIN_DIRS"),
            ("plugin dir holding a denied path", |c| c.plugin_dirs[1] = "/srv".into(), "CLAUDE_PLUGIN_DIRS"),
            ("plugin dir under the work dir", |c| c.plugin_dirs[0] = "/var/heinzel/plugins".into(), "CLAUDE_PLUGIN_DIRS"),
            ("work dir under HOME", |c| c.work_dir = "/home/heinzel/work".into(), "INVESTIGATION_WORK_DIR"),
            ("work dir under a denied path", |c| c.work_dir = "/srv/data/work".into(), "INVESTIGATION_WORK_DIR"),
            ("claude binary directly in HOME", |c| c.claude_bin = "/home/heinzel/claude".into(), "CLAUDE_BIN"),
            ("claude binary dir holding a denied path", |c| c.claude_bin = "/srv/claude".into(), "CLAUDE_BIN"),
        ];
        for (name, change, variable) in cases {
            let mut cfg = read_paths_config();
            change(&mut cfg);

            let err = cfg.resolve_read_paths().unwrap_err();

            assert!(err.starts_with(&format!("{variable}: ")), "{name}: got {err:?}");
        }
    }

    #[test]
    fn read_path_errors_read_like_go_s() {
        let mut cfg = read_paths_config();
        cfg.work_dir = "/home/heinzel/work".into();

        assert_eq!(
            cfg.resolve_read_paths().unwrap_err(),
            "INVESTIGATION_WORK_DIR: /home/heinzel/work lies under /home/heinzel, which the child may not read (HOME or INVESTIGATION_DENY_READ)"
        );
    }

    #[test]
    fn path_dirs_under_denied_paths_are_found_unless_reopened() {
        let mut cfg = read_paths_config();
        cfg.path = "/usr/bin::/home/heinzel/bin:/home/heinzel/.local/bin:/var/heinzel/tools".into();
        cfg.resolve_read_paths().unwrap();

        assert_eq!(
            cfg.denied_path_dirs(),
            [
                ("/home/heinzel/bin".to_owned(), "/home/heinzel".to_owned()),
                ("/var/heinzel/tools".to_owned(), "/var/heinzel".to_owned()),
            ]
        );
    }
}
