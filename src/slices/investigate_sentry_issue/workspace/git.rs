//! Runs git with a fixed environment and a timeout per command.

use std::os::fd::OwnedFd;
use std::process::Stdio;
use std::time::Duration;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use tokio::io::AsyncReadExt;
use tokio::net::unix::pipe;
use tokio::process::Command;
use tokio_util::sync::CancellationToken;

use super::super::process::exit_reason;
use super::Error;

const NETWORK_TIMEOUT: Duration = Duration::from_secs(10 * 60);
const LOCAL_TIMEOUT: Duration = Duration::from_secs(60);
const WAIT_DELAY: Duration = Duration::from_secs(10);
/// How short a cut-off token form at the end of git's output may be and
/// still be redacted; shorter ones match ordinary text too often.
const MIN_PARTIAL_TOKEN: usize = 8;
/// How much of git's output an error keeps.
const OUTPUT_MAX: usize = 2 << 10;
/// Transfers to GitHub slower than this many bytes per second for
/// [`LOW_SPEED_TIME`] seconds are aborted.
const LOW_SPEED_LIMIT: &str = "1000";
const LOW_SPEED_TIME: &str = "60";

/// Runs git with a fixed environment, adding the GitHub token as an HTTP
/// header only for the commands that talk to GitHub. Every command runs
/// under a timeout: `network_timeout` for clone, fetch and push,
/// `local_timeout` for the rest.
#[derive(Clone, Default)]
pub(super) struct GitRunner {
    path: String,
    home: String,
    token: String,
    pub(super) network_timeout: Duration,
    pub(super) local_timeout: Duration,
    /// Bounds how long a command's output is still read after it exited or
    /// was killed, e.g. while a lingering `git-remote-https` holds the pipe.
    pub(super) wait_delay: Duration,
}

impl std::fmt::Debug for GitRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GitRunner")
            .field("path", &self.path)
            .field("home", &self.home)
            .field("token", &"[redacted]")
            .field("network_timeout", &self.network_timeout)
            .field("local_timeout", &self.local_timeout)
            .field("wait_delay", &self.wait_delay)
            .finish()
    }
}

/// Why a command was stopped.
enum Stopped {
    Deadline,
    Canceled,
}

impl GitRunner {
    pub(super) fn new(path: &str, home: &str, token: &str) -> GitRunner {
        GitRunner {
            path: path.to_owned(),
            home: home.to_owned(),
            token: token.to_owned(),
            network_timeout: NETWORK_TIMEOUT,
            local_timeout: LOCAL_TIMEOUT,
            wait_delay: WAIT_DELAY,
        }
    }

    /// How long the git command `args` may run.
    pub(super) fn timeout(&self, args: &[&str]) -> Duration {
        match subcommand(args) {
            "clone" | "fetch" | "push" => self.network_timeout,
            _ => self.local_timeout,
        }
    }

    /// Runs git in `dir` and returns its trimmed output. With `auth`, the
    /// token reaches git only through the environment (`GIT_CONFIG_*`), never
    /// argv, a remote URL or a config file. Errors carry git's output, with
    /// the token redacted. Cancelling `cancel` kills the command.
    pub(super) async fn run(
        &self,
        dir: &str,
        auth: bool,
        args: &[&str],
        cancel: &CancellationToken,
    ) -> Result<String, Error> {
        self.run_with_env(dir, auth, args, &[], cancel).await
    }

    /// Runs git as [`GitRunner::run`] does, with `extra_env` added to its
    /// environment.
    pub(super) async fn run_with_env(
        &self,
        dir: &str,
        auth: bool,
        args: &[&str],
        extra_env: &[(&str, &str)],
        cancel: &CancellationToken,
    ) -> Result<String, Error> {
        let fail = |cause: String, output: &str| {
            Error(format!("git {}: {cause}: {}", subcommand(args), self.redact(output)))
        };
        if cancel.is_cancelled() {
            // Worded as Go's exec words a command its context stopped from
            // starting.
            return Err(fail("context canceled (context canceled)".to_owned(), ""));
        }
        // stdout and stderr share one pipe, so the output keeps git's order,
        // as Go's CombinedOutput does.
        let (reader, writer) = std::io::pipe().map_err(|err| fail(err.to_string(), ""))?;
        let mut output = pipe::Receiver::from_owned_fd(OwnedFd::from(reader))
            .map_err(|err| fail(err.to_string(), ""))?;
        let stderr = writer.try_clone().map_err(|err| fail(err.to_string(), ""))?;
        let mut command = Command::new("git");
        command
            .args(args)
            .current_dir(dir)
            .env_clear()
            .envs(self.env(auth))
            .envs(extra_env.iter().copied())
            .stdin(Stdio::null())
            .stdout(writer)
            .stderr(stderr)
            .kill_on_drop(true);
        let spawned = command.spawn();
        // Close our copies of the write end, so the read sees EOF once git
        // and whatever it started are done.
        drop(command);
        let mut child = spawned.map_err(|err| fail(err.to_string(), ""))?;

        let mut buf = Vec::new();
        let (status, stopped) = {
            let reading = output.read_to_end(&mut buf);
            tokio::pin!(reading);
            let mut read_done = false;
            let deadline = tokio::time::sleep(self.timeout(args));
            tokio::pin!(deadline);
            let stopped = loop {
                tokio::select! {
                    _ = &mut reading, if !read_done => read_done = true,
                    _ = child.wait() => break None,
                    () = &mut deadline => break Some(Stopped::Deadline),
                    () = cancel.cancelled() => break Some(Stopped::Canceled),
                }
            };
            if stopped.is_some() {
                let _ = child.start_kill();
            }
            let status = child.wait().await;
            if !read_done {
                let _ = tokio::time::timeout(self.wait_delay, &mut reading).await;
            }
            (status, stopped)
        };
        let text = String::from_utf8_lossy(&buf);
        let text = text.trim();
        let status = status.map_err(|err| fail(err.to_string(), text))?;
        match stopped {
            Some(Stopped::Deadline) => Err(fail(
                format!("context deadline exceeded ({})", exit_reason(status)),
                text,
            )),
            Some(Stopped::Canceled) => Err(fail(
                format!("context canceled ({})", exit_reason(status)),
                text,
            )),
            None if !status.success() => Err(fail(exit_reason(status), text)),
            None => Ok(text.to_owned()),
        }
    }

    /// Redacts the token in every form git could echo (as the Authorization
    /// header's base64, as bare base64 and raw) throughout `output`, then
    /// keeps the last [`OUTPUT_MAX`] bytes. Output that ends partway into a
    /// token form, as when git was killed mid-write, has that part redacted
    /// too.
    pub(super) fn redact(&self, output: &str) -> String {
        let mut text = output.to_owned();
        if !self.token.is_empty() {
            let forms = [
                BASE64.encode(format!("x-access-token:{}", self.token)),
                BASE64.encode(&self.token),
                self.token.clone(),
            ];
            for form in &forms {
                text = text.replace(form, "[redacted]");
            }
            if let Some(cut) = forms.iter().find_map(|form| partial_suffix(&text, form)) {
                text.truncate(text.len() - cut);
                text.push_str("[redacted]");
            }
        }
        let mut start = text.len().saturating_sub(OUTPUT_MAX);
        while !text.is_char_boundary(start) {
            start += 1;
        }
        text[start..].to_owned()
    }

    /// git's whole environment. With `auth` (the commands that talk to
    /// GitHub), it adds the token as an HTTP header and aborts transfers
    /// that stay below [`LOW_SPEED_LIMIT`] bytes/s for [`LOW_SPEED_TIME`]
    /// seconds.
    pub(super) fn env(&self, auth: bool) -> Vec<(String, String)> {
        let mut env = vec![
            ("PATH".to_owned(), self.path.clone()),
            ("HOME".to_owned(), self.home.clone()),
            ("GIT_TERMINAL_PROMPT".to_owned(), "0".to_owned()),
            ("GIT_CONFIG_NOSYSTEM".to_owned(), "1".to_owned()),
            ("GIT_CONFIG_GLOBAL".to_owned(), "/dev/null".to_owned()),
        ];
        if !auth {
            return env;
        }
        let credentials = BASE64.encode(format!("x-access-token:{}", self.token));
        let config = [
            ("http.extraHeader", format!("Authorization: Basic {credentials}")),
            ("http.lowSpeedLimit", LOW_SPEED_LIMIT.to_owned()),
            ("http.lowSpeedTime", LOW_SPEED_TIME.to_owned()),
        ];
        env.push(("GIT_CONFIG_COUNT".to_owned(), config.len().to_string()));
        for (i, (key, value)) in config.into_iter().enumerate() {
            env.push((format!("GIT_CONFIG_KEY_{i}"), key.to_owned()));
            env.push((format!("GIT_CONFIG_VALUE_{i}"), value));
        }
        env
    }
}

/// The length of the longest start of `form`, at least
/// [`MIN_PARTIAL_TOKEN`] bytes, that `text` ends with.
fn partial_suffix(text: &str, form: &str) -> Option<usize> {
    (MIN_PARTIAL_TOKEN..form.len())
        .rev()
        .find(|&n| text.as_bytes().ends_with(&form.as_bytes()[..n]))
}

/// Returns the git subcommand in `args`, skipping global options.
fn subcommand<'a>(args: &[&'a str]) -> &'a str {
    let mut args = args.iter();
    while let Some(arg) = args.next() {
        match *arg {
            "-c" | "-C" => {
                args.next();
            }
            arg if !arg.starts_with('-') => return arg,
            _ => {}
        }
    }
    ""
}
