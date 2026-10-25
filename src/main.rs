//! Command heinzel runs the event-sourced app until SIGINT or SIGTERM.
//!
//! Exit codes: 0 after a clean shutdown (or `--help`), 2 for invalid
//! arguments or configuration, 1 for runtime errors.
#![forbid(unsafe_code)]

use std::process::ExitCode;
use std::time::Duration;

use clap::Parser;
use clap::error::ErrorKind;
use heinzel::app::{self, Config, Deps};
use tokio::net::TcpListener;
use tokio::signal::unix::{SignalKind, signal};
use tokio_util::sync::CancellationToken;

const INVALID: u8 = 2;

fn main() -> ExitCode {
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(err) => {
            eprintln!("start the async runtime: {err}");
            return ExitCode::FAILURE;
        }
    };
    let code = runtime.block_on(heinzel());
    // Don't wait for tasks still stuck in sync code, such as a request cut
    // off at the shutdown timeout: exit right away, as Go does.
    runtime.shutdown_timeout(Duration::ZERO);
    code
}

async fn heinzel() -> ExitCode {
    if let Err(err) =
        tracing::subscriber::set_global_default(heinzel::logging::json(std::io::stderr))
    {
        eprintln!("set up logging: {err}");
        return ExitCode::FAILURE;
    }

    let config = match Config::try_parse() {
        Ok(config) => config,
        Err(err)
            if matches!(
                err.kind(),
                ErrorKind::DisplayHelp | ErrorKind::DisplayVersion
            ) =>
        {
            let _ = err.print();
            return ExitCode::SUCCESS;
        }
        Err(err) => {
            let _ = err.print();
            tracing::error!(error = first_line(&err), "invalid arguments");
            return ExitCode::from(INVALID);
        }
    };
    let getenv = |key: &str| std::env::var(key).ok();
    let slices = match app::build(heinzel::slices::FACTORIES, &Deps { getenv: &getenv }) {
        Ok(slices) => slices,
        Err(err) => {
            tracing::error!(error = %err, "invalid configuration");
            return ExitCode::from(INVALID);
        }
    };

    let shutdown = CancellationToken::new();
    if let Err(err) = cancel_on_signal(shutdown.clone()) {
        tracing::error!(error = %err, "install signal handlers");
        return ExitCode::FAILURE;
    }
    let addr = config.listen_addr();
    let listener = match TcpListener::bind(&addr).await {
        Ok(listener) => listener,
        Err(err) => {
            tracing::error!(addr = %config.addr, error = %err, "listen");
            return ExitCode::FAILURE;
        }
    };
    if let Err(err) = app::run(&config, listener, slices, shutdown).await {
        tracing::error!(error = %err, "app stopped with error");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

/// Cancels `shutdown` on the first SIGINT or SIGTERM.
fn cancel_on_signal(shutdown: CancellationToken) -> std::io::Result<()> {
    let mut interrupt = signal(SignalKind::interrupt())?;
    let mut terminate = signal(SignalKind::terminate())?;
    tokio::spawn(async move {
        tokio::select! {
            _ = interrupt.recv() => {}
            _ = terminate.recv() => {}
        }
        shutdown.cancel();
    });
    Ok(())
}

/// Returns clap's message without its "error: " prefix and usage hint.
fn first_line(err: &clap::Error) -> String {
    let rendered = err.to_string();
    let line = rendered.lines().next().unwrap_or_default();
    line.strip_prefix("error: ").unwrap_or(line).to_owned()
}
