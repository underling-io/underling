//! Runs headless Claude Code in a sandboxed child process: builds the
//! command line, environment and sandbox settings for a run in a workspace,
//! runs the child and reads its stream-json output.

mod run;
mod stream;

#[cfg(test)]
mod tests;

use serde::Serialize;

use super::paths::with_resolved;
use super::workspace::Workspace;

pub(crate) use run::{RunError, run};

/// The only host sandboxed commands may reach: the Codex CLI calls the
/// OpenAI API with `OPENAI_API_KEY`.
const OPENAI_HOST: &str = "api.openai.com";

/// What a claude run needs from heinzel's settings.
#[derive(Clone, Default)]
pub(crate) struct Config {
    pub(crate) bin: String,
    pub(crate) plugin_dirs: Vec<String>,
    /// The `PATH` handed to the child.
    pub(crate) path: String,
    pub(crate) anthropic_key: String,
    pub(crate) openai_key: String,
    /// heinzel's own `HOME`; empty if unset. Neither the child's file tools
    /// nor its sandboxed commands may read it.
    pub(crate) parent_home: String,
    /// More paths the child may not read, such as heinzel's data dir.
    pub(crate) deny_read: Vec<String>,
    /// The paths under denied ones that sandboxed commands still need to
    /// read: the plugin dirs and the claude binary's dir.
    pub(crate) readable: Vec<String>,
}

impl std::fmt::Debug for Config {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Config")
            .field("bin", &self.bin)
            .field("plugin_dirs", &self.plugin_dirs)
            .field("path", &self.path)
            .field("anthropic_key", &"[redacted]")
            .field("openai_key", &"[redacted]")
            .field("parent_home", &self.parent_home)
            .field("deny_read", &self.deny_read)
            .field("readable", &self.readable)
            .finish()
    }
}

/// One invocation of the claude CLI.
#[derive(Clone)]
pub(crate) struct Cmd {
    bin: String,
    args: Vec<String>,
    env: Vec<(String, String)>,
    dir: String,
    /// The prompt. `-p` reads it from stdin when argv has none, so it never
    /// shows in the process list or meets argv size limits.
    stdin: String,
    /// The `--plugin-dir` values the session must report as loaded.
    plugin_dirs: Vec<String>,
}

impl std::fmt::Debug for Cmd {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let env: Vec<&str> = self.env.iter().map(|(k, _)| k.as_str()).collect();
        f.debug_struct("Cmd")
            .field("bin", &self.bin)
            .field("args", &self.args)
            .field("env (values redacted)", &env)
            .field("dir", &self.dir)
            .field("plugin_dirs", &self.plugin_dirs)
            .finish_non_exhaustive()
    }
}

/// Builds the headless claude invocation for `ws`. The child gets only
/// `PATH`, its own `HOME` and config dir, and the two model credentials;
/// nothing else from heinzel's environment.
pub(crate) fn command(cfg: &Config, ws: &Workspace, prompt: &str) -> Cmd {
    let settings = sandbox_settings(cfg, ws);
    let mut args: Vec<String> = [
        "--bare",
        "-p",
        "--verbose",
        "--output-format",
        "stream-json",
        "--no-session-persistence",
        "--setting-sources",
        "",
        "--settings",
        &settings,
        "--permission-mode",
        "acceptEdits",
        "--permission-prompts",
        "none",
    ]
    .map(String::from)
    .into();
    for dir in &cfg.plugin_dirs {
        args.extend(["--plugin-dir".to_owned(), dir.clone()]);
    }
    Cmd {
        bin: cfg.bin.clone(),
        args,
        env: [
            ("PATH", cfg.path.as_str()),
            ("HOME", &ws.home),
            ("CLAUDE_CONFIG_DIR", &ws.claude_config),
            ("ANTHROPIC_API_KEY", &cfg.anthropic_key),
            ("OPENAI_API_KEY", &cfg.openai_key),
        ]
        .map(|(k, v)| (k.to_owned(), v.to_owned()))
        .into(),
        dir: ws.clone.clone(),
        stdin: prompt.to_owned(),
        plugin_dirs: cfg.plugin_dirs.clone(),
    }
}

/// The subset of Claude Code's settings.json heinzel generates, in the
/// field order the JSON keeps.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Settings {
    permissions: Permissions,
    sandbox: Sandbox,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Permissions {
    default_mode: &'static str,
    disable_bypass_permissions_mode: &'static str,
    deny: Vec<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Sandbox {
    enabled: bool,
    fail_if_unavailable: bool,
    auto_allow_bash_if_sandboxed: bool,
    allow_unsandboxed_commands: bool,
    filesystem: Filesystem,
    network: Network,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Filesystem {
    allow_write: Vec<String>,
    deny_read: Vec<String>,
    allow_read: Vec<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Network {
    allowed_domains: Vec<String>,
}

/// Returns the settings JSON for a run in `ws`. Shell commands run in
/// Claude Code's sandbox, which refuses to start unsandboxed, never retries
/// outside it, may write only to the clone and the child's `HOME` (plus the
/// sandbox's own temp dir) and may reach only the OpenAI API. File edits are
/// auto-approved inside the working directory (the clone) only; anything
/// that would need a prompt is denied, since nobody can answer one.
///
/// Reads are denied for heinzel's `HOME`, the configured deny-read paths
/// and the whole work dir. Sandboxed commands get the clone, the child's
/// `HOME` and Claude config dir (shell snapshots, plugin data), the plugin
/// dirs and the claude binary's dir re-opened (a narrower allowRead wins
/// over a broader denyRead). Read deny rules can't be carved out, so for the
/// file tools the work dir is denied piecemeal: everything in it but this
/// run's clone, `HOME` and Claude config dir.
fn sandbox_settings(cfg: &Config, ws: &Workspace) -> String {
    let mut denied_input = vec![cfg.parent_home.clone()];
    denied_input.extend(cfg.deny_read.iter().cloned());
    let denied = with_resolved(&denied_input);
    let work_paths: Vec<String> = ws
        .siblings
        .iter()
        .chain([&ws.mirror, &ws.git_home])
        .flat_map(|p| as_given_and_resolved(ws, p))
        .collect();

    // The file tools are kept out by deny rules. Edit rules too, since
    // Claude Code's own check of where edits may go is the only other thing
    // between the Edit tool and, say, the mirror's config.
    let mut deny = vec!["Read(//proc/**)".to_owned()];
    for p in denied.iter().chain(&work_paths) {
        deny.push(deny_rule("Read", p));
        deny.push(deny_rule("Edit", p));
    }
    let mut deny_read = denied;
    deny_read.extend(as_given_and_resolved(ws, &ws.work_dir));
    let mut allow_read = vec![ws.clone.clone(), ws.home.clone(), ws.claude_config.clone()];
    allow_read.extend(cfg.readable.iter().cloned());

    let settings = Settings {
        permissions: Permissions {
            default_mode: "acceptEdits",
            disable_bypass_permissions_mode: "disable",
            deny,
        },
        sandbox: Sandbox {
            enabled: true,
            fail_if_unavailable: true,
            auto_allow_bash_if_sandboxed: true,
            allow_unsandboxed_commands: false,
            filesystem: Filesystem {
                allow_write: vec![ws.clone.clone(), ws.home.clone()],
                deny_read,
                allow_read,
            },
            network: Network {
                allowed_domains: vec![OPENAI_HOST.to_owned()],
            },
        },
    };
    // Serialising strings, bools and lists can't fail.
    serde_json::to_string(&settings).unwrap_or_default()
}

/// The permission rule that keeps Claude's `tool` out of `dir`, an absolute
/// path: rules mark absolute paths with a leading `//`.
fn deny_rule(tool: &str, dir: &str) -> String {
    format!("{tool}(/{dir}/**)")
}

/// `path`, a path in the work dir with symlinks resolved, preceded by its
/// form under the work dir as configured when that differs, so the child is
/// kept out whichever form it reaches the path by.
fn as_given_and_resolved(ws: &Workspace, path: &str) -> Vec<String> {
    match path.strip_prefix(&ws.work_dir) {
        Some(rest)
            if !ws.given_work_dir.is_empty()
                && ws.given_work_dir != ws.work_dir
                && (rest.is_empty() || rest.starts_with('/')) =>
        {
            vec![format!("{}{rest}", ws.given_work_dir), path.to_owned()]
        }
        _ => vec![path.to_owned()],
    }
}
