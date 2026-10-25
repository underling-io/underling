//! Runs the claude child until it exits, times out or heinzel shuts down.

use std::process::Stdio;
use std::time::Duration;

use nix::sys::signal::{Signal, killpg};
use nix::unistd::Pid;
use tokio::io::{AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::Command;
use tokio_util::sync::CancellationToken;

use super::super::process::exit_reason;
use super::super::paths::{clean, resolve_path, within};
use super::stream::{InitEvent, StreamOutput, read_stream};
use super::Cmd;

/// Bounds how long heinzel reads the child's output after the child exited
/// and its process group was killed.
pub(super) const DRAIN_TIMEOUT: Duration = Duration::from_secs(5);
/// How much of the child's stderr a failure reason keeps.
const STDERR_TAIL: usize = 2 << 10;

/// Why a claude run failed.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub(crate) enum RunError {
    /// heinzel began shutting down; the child was killed.
    #[error("heinzel shut down while claude was running")]
    Shutdown,
    /// The child ran past its timeout and was killed.
    #[error("claude did not finish within {}", go_duration(*.0))]
    Timeout(Duration),
    /// Anything else, with the reason.
    #[error("{0}")]
    Failed(String),
}

/// Why the child was stopped.
#[derive(PartialEq, Eq)]
enum Stopped {
    Deadline,
    Canceled,
}

/// Runs `cmd` until it exits, `timeout` passes or `cancel` fires, killing
/// it and its process group in the latter cases, and returns Claude's
/// summary. Whatever the child leaves running in its group is killed when
/// it exits, so no tool outlives the run or overlaps the push.
pub(crate) async fn run(
    cmd: &Cmd,
    timeout: Duration,
    cancel: &CancellationToken,
) -> Result<String, RunError> {
    if cancel.is_cancelled() {
        return Err(RunError::Shutdown);
    }
    let mut child = Command::new(&cmd.bin)
        .args(&cmd.args)
        .current_dir(&cmd.dir)
        .env_clear()
        .envs(cmd.env.iter().map(|(k, v)| (k, v)))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .kill_on_drop(true)
        .spawn()
        .map_err(|err| RunError::Failed(format!("start claude: {err}")))?;
    let group = child
        .id()
        .and_then(|pid| i32::try_from(pid).ok())
        .map(Pid::from_raw);
    // Kills the group however this function ends, panics and a dropped
    // future included.
    let mut kill_guard = KillGroupOnDrop(group);
    let (stdin, stdout, stderr) = (child.stdin.take(), child.stdout.take(), child.stderr.take());

    let mut out = StreamOutput::default();
    let mut stderr_tail = TailBuffer::default();
    let (status, stopped, read_result) = {
        let feed = async {
            if let Some(mut stdin) = stdin {
                // A child that exits without reading its prompt closes the
                // pipe; that's for its exit status to tell.
                let _ = stdin.write_all(cmd.stdin.as_bytes()).await;
            }
        };
        let read_out = async {
            match stdout {
                Some(stdout) => read_stream(BufReader::new(stdout), &mut out).await,
                None => Ok(()),
            }
        };
        let read_err = async {
            if let Some(mut stderr) = stderr {
                let mut chunk = [0u8; 8192];
                while let Ok(n @ 1..) = stderr.read(&mut chunk).await {
                    stderr_tail.push(&chunk[..n]);
                }
            }
        };
        let pipes = async {
            let ((), read, ()) = tokio::join!(feed, read_out, read_err);
            read
        };
        tokio::pin!(pipes);
        let mut read_result = None;
        let deadline = tokio::time::sleep(timeout);
        tokio::pin!(deadline);
        let stopped = loop {
            tokio::select! {
                read = &mut pipes, if read_result.is_none() => read_result = Some(read),
                _ = child.wait() => break None,
                () = &mut deadline => break Some(Stopped::Deadline),
                () = cancel.cancelled() => break Some(Stopped::Canceled),
            }
        };
        if stopped.is_some() {
            kill_group(group);
        }
        let status = child.wait().await;
        kill_group(group);
        // The group is dealt with; once it's empty its id may be reused, so
        // the guard must not kill it again later.
        kill_guard.disarm();
        if read_result.is_none() {
            // Only something outside the group can still hold the pipes;
            // give up on it and keep what was read.
            read_result = tokio::time::timeout(DRAIN_TIMEOUT, &mut pipes).await.ok();
        }
        (status, stopped, read_result.unwrap_or(Ok(())))
    };

    if cancel.is_cancelled() {
        return Err(RunError::Shutdown);
    }
    if stopped == Some(Stopped::Deadline) {
        return Err(RunError::Timeout(timeout));
    }
    let stderr_text = stderr_tail.text();
    let status = status.map_err(|err| RunError::Failed(format!("claude: {err}")))?;
    if !status.success() {
        return Err(RunError::Failed(join_reason(&[
            &format!("claude: {}", exit_reason(status)),
            out.summary(),
            &stderr_text,
        ])));
    }
    read_result.map_err(|err| RunError::Failed(format!("read claude output: {err}")))?;
    check_init(out.init.as_ref(), &cmd.plugin_dirs).map_err(|err| {
        RunError::Failed(format!("claude's session did not start as configured: {err}"))
    })?;
    if !out.ok() {
        return Err(RunError::Failed(join_reason(&[
            "claude did not end with a successful result",
            out.summary(),
            &stderr_text,
        ])));
    }
    Ok(out.summary().to_owned())
}

/// Kills the child's process group, if it still has members.
fn kill_group(group: Option<Pid>) {
    if let Some(group) = group {
        let _ = killpg(group, Signal::SIGKILL);
    }
}

/// Kills the child's process group when dropped.
struct KillGroupOnDrop(Option<Pid>);

impl KillGroupOnDrop {
    fn disarm(&mut self) {
        self.0 = None;
    }
}

impl Drop for KillGroupOnDrop {
    fn drop(&mut self) {
        kill_group(self.0);
    }
}

/// Keeps the last [`STDERR_TAIL`] bytes written to it.
#[derive(Default)]
struct TailBuffer(Vec<u8>);

impl TailBuffer {
    fn push(&mut self, bytes: &[u8]) {
        self.0.extend_from_slice(bytes);
        if let Some(over) = self.0.len().checked_sub(STDERR_TAIL) {
            self.0.drain(..over);
        }
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.0).into_owned()
    }
}

/// Checks that claude's session started as heinzel configured it: in
/// acceptEdits mode, with a plugin loaded from each of `plugin_dirs` and no
/// plugin errors. The init event is the only evidence that `--settings` and
/// `--plugin-dir` took effect: `-p` silently ignores invalid settings.
pub(super) fn check_init(init: Option<&InitEvent>, plugin_dirs: &[String]) -> Result<(), String> {
    let init = init.ok_or("no system/init event")?;
    let mode = init.permission_mode.as_deref().unwrap_or_default();
    if mode != "acceptEdits" {
        return Err(format!("permission mode {mode:?}, not acceptEdits"));
    }
    let plugins = init.plugins.as_deref().unwrap_or_default();
    for dir in plugin_dirs {
        let loaded = plugins
            .iter()
            .any(|p| loaded_from(p.path.as_deref().unwrap_or_default(), dir));
        if !loaded {
            return Err(format!("no plugin loaded from {dir}"));
        }
    }
    let errors = init.plugin_errors.as_deref().unwrap_or_default();
    if !errors.is_empty() {
        let messages: Vec<&str> = errors.iter().map(|e| e.get()).collect();
        return Err(format!("plugin errors: {}", messages.join("; ")));
    }
    Ok(())
}

/// Reports whether a plugin at `path` came from the `--plugin-dir` `dir`:
/// the dir itself, or a plugin inside a folder of plugins.
fn loaded_from(path: &str, dir: &str) -> bool {
    within(&clean(path), &clean(dir)) || within(&resolve_path(path), &resolve_path(dir))
}

/// Joins the non-blank parts of a failure reason.
fn join_reason(parts: &[&str]) -> String {
    parts
        .iter()
        .map(|p| p.trim())
        .filter(|p| !p.is_empty())
        .collect::<Vec<_>>()
        .join(": ")
}

/// Formats `d` as Go's `time.Duration.String` does: `30m0s`, `1h0m0s`,
/// `1m30s`, `1.5s`, `500ms`.
pub(super) fn go_duration(d: Duration) -> String {
    let nanos = d.as_nanos();
    if nanos == 0 {
        return "0s".to_owned();
    }
    if nanos < 1_000 {
        return format!("{nanos}ns");
    }
    if nanos < 1_000_000 {
        return format!("{}µs", decimal(nanos, 1_000));
    }
    if nanos < 1_000_000_000 {
        return format!("{}ms", decimal(nanos, 1_000_000));
    }
    let secs = nanos / 1_000_000_000;
    let (hours, minutes) = (secs / 3600, secs / 60 % 60);
    let seconds = decimal(nanos % 60_000_000_000, 1_000_000_000);
    match (hours, minutes) {
        (0, 0) => format!("{seconds}s"),
        (0, m) => format!("{m}m{seconds}s"),
        (h, m) => format!("{h}h{m}m{seconds}s"),
    }
}

/// `value / scale` as a decimal without trailing zeros; `scale` is a power
/// of ten.
fn decimal(value: u128, scale: u128) -> String {
    let (whole, fraction) = (value / scale, value % scale);
    if fraction == 0 {
        return whole.to_string();
    }
    let width = scale.ilog10() as usize;
    let fraction = format!("{fraction:0width$}");
    format!("{whole}.{}", fraction.trim_end_matches('0'))
}
