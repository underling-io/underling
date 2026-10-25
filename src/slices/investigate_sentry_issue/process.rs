//! How a child process ended, worded as Go words it, since that text ends up
//! in persisted failure reasons.

use std::os::unix::process::ExitStatusExt;
use std::process::ExitStatus;

/// Describes how a process ended, like Go's `ProcessState.String`:
/// `exit status 1`, `signal: killed`.
pub(crate) fn exit_reason(status: ExitStatus) -> String {
    if let Some(code) = status.code() {
        return format!("exit status {code}");
    }
    let signal = status.signal().unwrap_or_default();
    let name = match signal {
        1 => "hangup".to_owned(),
        2 => "interrupt".to_owned(),
        9 => "killed".to_owned(),
        11 => "segmentation fault".to_owned(),
        13 => "broken pipe".to_owned(),
        15 => "terminated".to_owned(),
        other => format!("signal {other}"),
    };
    if status.core_dumped() {
        format!("signal: {name} (core dumped)")
    } else {
        format!("signal: {name}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exit_reasons_read_like_go_s() {
        assert_eq!(exit_reason(ExitStatus::from_raw(0)), "exit status 0");
        assert_eq!(exit_reason(ExitStatus::from_raw(128 << 8)), "exit status 128");
        assert_eq!(exit_reason(ExitStatus::from_raw(9)), "signal: killed");
        assert_eq!(exit_reason(ExitStatus::from_raw(15)), "signal: terminated");
        assert_eq!(exit_reason(ExitStatus::from_raw(42)), "signal: signal 42");
    }
}
