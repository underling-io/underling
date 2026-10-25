//! The app's command-line settings.

use std::path::PathBuf;
use std::time::Duration;

/// The app's command-line settings.
#[derive(Debug, Clone, PartialEq, Eq, clap::Parser)]
#[command(name = "heinzel", version, about = "Runs the event-sourced app.")]
pub struct Config {
    /// HTTP listen address
    #[arg(long, default_value = ":8080")]
    pub addr: String,
    /// Directory holding the event store
    #[arg(long, default_value = "./data")]
    pub data_dir: PathBuf,
    /// How long in-flight requests may take to finish on shutdown, e.g. 10s
    #[arg(long, default_value = "10s", value_parser = humantime::parse_duration)]
    pub shutdown_timeout: Duration,
}

impl Config {
    /// The address to bind: `addr`, with a missing host (as in `:8080`)
    /// meaning every IPv4 interface.
    pub fn listen_addr(&self) -> String {
        match self.addr.strip_prefix(':') {
            Some(port) => format!("0.0.0.0:{port}"),
            None => self.addr.clone(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;

    fn parse(args: &[&str]) -> Result<Config, clap::Error> {
        Config::try_parse_from(std::iter::once("heinzel").chain(args.iter().copied()))
    }

    #[test]
    fn defaults() {
        let got = parse(&[]).unwrap();

        let want = Config {
            addr: ":8080".into(),
            data_dir: "./data".into(),
            shutdown_timeout: Duration::from_secs(10),
        };
        assert_eq!(got, want);
    }

    #[test]
    fn overrides() {
        let got = parse(&[
            "--addr",
            "127.0.0.1:9000",
            "--data-dir",
            "/var/lib/heinzel",
            "--shutdown-timeout",
            "1m30s",
        ])
        .unwrap();

        let want = Config {
            addr: "127.0.0.1:9000".into(),
            data_dir: "/var/lib/heinzel".into(),
            shutdown_timeout: Duration::from_secs(90),
        };
        assert_eq!(got, want);
    }

    #[test]
    fn rejects_unknown_flags() {
        assert!(parse(&["--nope"]).is_err());
    }

    #[test]
    fn rejects_positional_arguments() {
        assert!(parse(&["serve"]).is_err());
    }

    #[test]
    fn rejects_an_invalid_duration() {
        assert!(parse(&["--shutdown-timeout", "soon"]).is_err());
    }

    #[test]
    fn listen_addr_fills_in_a_missing_host() {
        let config = |addr: &str| Config {
            addr: addr.into(),
            ..parse(&[]).unwrap()
        };

        assert_eq!(config(":8080").listen_addr(), "0.0.0.0:8080");
        assert_eq!(config("127.0.0.1:9000").listen_addr(), "127.0.0.1:9000");
    }
}
