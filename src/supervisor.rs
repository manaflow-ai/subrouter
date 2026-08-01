//! Stable front listener and zero-downtime worker replacement.

#[cfg(unix)]
mod unix {
    use std::collections::HashMap;
    use std::fs;
    use std::future::IntoFuture as _;
    use std::os::unix::fs::{FileTypeExt as _, PermissionsExt as _};
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use anyhow::{Context, anyhow, bail};
    use axum::extract::State;
    use axum::http::StatusCode;
    use axum::response::{IntoResponse, Response};
    use axum::routing::{get, post};
    use axum::{Json, Router as AxumRouter};
    use clap::Parser;
    use nix::errno::Errno;
    use nix::sys::signal::{Signal, kill};
    use nix::unistd::Pid;
    use serde_json::json;
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
    use tokio::net::{TcpListener, UnixListener, UnixStream};
    use tokio::process::Command;
    use tokio::sync::{Mutex as AsyncMutex, mpsc, watch};
    use tracing::{info, warn};

    use crate::front::{Backend, Router};

    #[derive(Clone, Parser)]
    #[command(name = "subrouter supervise", trailing_var_arg = true)]
    struct Options {
        #[arg(long, default_value = "127.0.0.1:31415")]
        addr: std::net::SocketAddr,
        #[arg(long, default_value = "/var/run/subrouter-supervisor.sock")]
        control_socket: PathBuf,
        #[arg(long)]
        worker_bin: PathBuf,
        #[arg(long, default_value = "30s", value_parser = parse_duration)]
        ready_timeout: Duration,
        #[arg(long, default_value = "10m", value_parser = parse_duration)]
        drain_timeout: Duration,
        #[arg(allow_hyphen_values = true)]
        worker_args: Vec<String>,
    }

    #[derive(Clone, Debug)]
    struct Worker {
        id: String,
        pid: u32,
        address: PathBuf,
        socket_dir: PathBuf,
        done: watch::Receiver<Option<String>>,
    }

    impl Worker {
        async fn wait(&self) -> String {
            let mut done = self.done.clone();
            loop {
                if let Some(status) = done.borrow().clone() {
                    return status;
                }
                if done.changed().await.is_err() {
                    return "worker status channel closed".into();
                }
            }
        }

        fn exited(&self) -> Option<String> {
            self.done.borrow().clone()
        }
    }

    struct Supervisor {
        options: Options,
        router: Router,
        workers: Mutex<HashMap<String, Worker>>,
        upgrade_lock: AsyncMutex<()>,
        stopping: AtomicBool,
        exit_tx: mpsc::UnboundedSender<(String, String)>,
    }

    pub async fn run(args: &[String]) -> anyhow::Result<()> {
        let mut options = Options::try_parse_from(
            std::iter::once("subrouter supervise").chain(args.iter().map(String::as_str)),
        )?;
        validate(&mut options)?;
        let (exit_tx, mut exit_rx) = mpsc::unbounded_channel();
        let initial = start_worker(&options, &exit_tx).await?;
        let router = Router::new(Backend {
            id: initial.id.clone(),
            network: "unix".into(),
            address: initial.address.to_string_lossy().into(),
        })?;
        let supervisor = Arc::new(Supervisor {
            options,
            router,
            workers: Mutex::new(HashMap::from([(initial.id.clone(), initial)])),
            upgrade_lock: AsyncMutex::new(()),
            stopping: AtomicBool::new(false),
            exit_tx,
        });
        supervisor.run_loop(&mut exit_rx).await
    }

    fn validate(options: &mut Options) -> anyhow::Result<()> {
        if !options.control_socket.is_absolute() {
            bail!("--control-socket must be an absolute path");
        }
        if options.worker_bin.as_os_str().is_empty() {
            bail!("--worker-bin is required");
        }
        if options.ready_timeout.is_zero() || options.drain_timeout.is_zero() {
            bail!("ready and drain timeouts must be positive");
        }
        if options
            .worker_args
            .first()
            .is_some_and(|value| value == "serve")
        {
            options.worker_args.remove(0);
        }
        if options
            .worker_args
            .iter()
            .any(|value| value == "--addr" || value.starts_with("--addr="))
        {
            bail!("worker arguments cannot set --addr; the supervisor owns worker listeners");
        }
        if options
            .worker_args
            .iter()
            .any(|value| value == "--worker-socket" || value.starts_with("--worker-socket="))
        {
            bail!("worker arguments cannot set --worker-socket");
        }
        Ok(())
    }

    impl Supervisor {
        async fn run_loop(
            self: Arc<Self>,
            exit_rx: &mut mpsc::UnboundedReceiver<(String, String)>,
        ) -> anyhow::Result<()> {
            let listener = TcpListener::bind(self.options.addr)
                .await
                .with_context(|| format!("listen on {}", self.options.addr))?;
            prepare_control_socket(&self.options.control_socket)?;
            let control_listener = UnixListener::bind(&self.options.control_socket)?;
            fs::set_permissions(
                &self.options.control_socket,
                fs::Permissions::from_mode(0o600),
            )?;
            let _cleanup = SocketCleanup(self.options.control_socket.clone());
            let control_app = AxumRouter::new()
                .route("/_subrouter/supervisor-status", get(control_status))
                .route("/_subrouter/upgrade", post(control_upgrade))
                .with_state(Arc::clone(&self));
            let front = self.router.serve(listener);
            let control = axum::serve(control_listener, control_app).into_future();
            tokio::pin!(front);
            tokio::pin!(control);

            let mut terminate =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
            let mut hangup =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::hangup())?;
            info!(addr = %self.options.addr, control_socket = %self.options.control_socket.display(), worker = %self.router.active().id, "subrouter supervisor listening");
            let result = loop {
                tokio::select! {
                    result = &mut front => break result.map_err(anyhow::Error::from),
                    result = &mut control => break result.map_err(anyhow::Error::from),
                    _ = tokio::signal::ctrl_c() => break Ok(()),
                    _ = terminate.recv() => break Ok(()),
                    _ = hangup.recv() => {
                        if let Err(error) = self.upgrade().await {
                            warn!(%error, "subrouter worker upgrade failed");
                        }
                    }
                    Some((id, status)) = exit_rx.recv() => {
                        if self.router.active().id == id && !self.stopping.load(Ordering::Acquire) {
                            warn!(generation = %id, %status, "active subrouter worker exited");
                            if let Err(error) = self.upgrade().await {
                                break Err(error.context("active worker recovery failed"));
                            }
                        }
                    }
                }
            };
            self.stop().await;
            result
        }

        async fn upgrade(self: &Arc<Self>) -> anyhow::Result<()> {
            let _guard = self.upgrade_lock.lock().await;
            if self.stopping.load(Ordering::Acquire) {
                bail!("supervisor is shutting down");
            }
            let next = start_worker(&self.options, &self.exit_tx).await?;
            let previous = self.router.active();
            self.workers
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .insert(next.id.clone(), next.clone());
            if let Err(error) = self.router.switch(Backend {
                id: next.id.clone(),
                network: "unix".into(),
                address: next.address.to_string_lossy().into(),
            }) {
                let _ = signal(next.pid, Signal::SIGTERM);
                return Err(error.into());
            }
            info!(from = %previous.id, to = %next.id, "subrouter worker switched");
            if let Some(worker) = self
                .workers
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .get(&previous.id)
                .cloned()
            {
                let _ = signal(worker.pid, Signal::SIGUSR1);
                tokio::spawn(Arc::clone(self).reap_when_idle(worker));
            }
            Ok(())
        }

        async fn reap_when_idle(self: Arc<Self>, worker: Worker) {
            self.router.wait_idle(&worker.id).await;
            let _ = signal(worker.pid, Signal::SIGTERM);
            if tokio::time::timeout(Duration::from_secs(5), worker.wait())
                .await
                .is_err()
            {
                let _ = signal(worker.pid, Signal::SIGKILL);
                let _ = worker.wait().await;
            }
            let _ = tokio::time::timeout(Duration::from_secs(1), self.router.wait_idle(&worker.id))
                .await;
            let _ = self.router.forget(&worker.id);
            self.workers
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .remove(&worker.id);
            let _ = fs::remove_dir_all(&worker.socket_dir);
        }

        async fn stop(&self) {
            self.stopping.store(true, Ordering::Release);
            let _ =
                tokio::time::timeout(self.options.drain_timeout, self.router.wait_all_idle()).await;
            let workers: Vec<_> = self
                .workers
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .values()
                .cloned()
                .collect();
            for worker in &workers {
                let _ = signal(worker.pid, Signal::SIGTERM);
            }
            for worker in workers {
                if tokio::time::timeout(Duration::from_secs(5), worker.wait())
                    .await
                    .is_err()
                {
                    let _ = signal(worker.pid, Signal::SIGKILL);
                }
                let _ = fs::remove_dir_all(worker.socket_dir);
            }
        }
    }

    async fn start_worker(
        options: &Options,
        exit_tx: &mpsc::UnboundedSender<(String, String)>,
    ) -> anyhow::Result<Worker> {
        let socket_dir = tempfile::Builder::new()
            .prefix("subrouter-worker-")
            .tempdir()?
            .keep();
        fs::set_permissions(&socket_dir, fs::Permissions::from_mode(0o700))?;
        let address = socket_dir.join("worker.sock");
        let mut command = Command::new(&options.worker_bin);
        command
            .arg("serve")
            .arg("--worker-socket")
            .arg(&address)
            .args(&options.worker_args);
        command
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::inherit())
            .stderr(std::process::Stdio::inherit());
        let mut child = command
            .spawn()
            .with_context(|| format!("start {}", options.worker_bin.display()))?;
        let pid = child
            .id()
            .ok_or_else(|| anyhow!("worker has no process id"))?;
        let id = format!(
            "{}-{pid}",
            chrono::Utc::now().timestamp_nanos_opt().unwrap_or_default()
        );
        let (done_tx, done) = watch::channel(None);
        let event_id = id.clone();
        let event_tx = exit_tx.clone();
        tokio::spawn(async move {
            let status = match child.wait().await {
                Ok(status) => status.to_string(),
                Err(error) => format!("wait failed: {error}"),
            };
            let _ = done_tx.send(Some(status.clone()));
            let _ = event_tx.send((event_id, status));
        });
        let worker = Worker {
            id,
            pid,
            address,
            socket_dir,
            done,
        };
        if let Err(error) = wait_ready(&worker, options.ready_timeout).await {
            let _ = signal(pid, Signal::SIGTERM);
            let _ = tokio::time::timeout(Duration::from_secs(1), worker.wait()).await;
            let _ = fs::remove_dir_all(&worker.socket_dir);
            return Err(error);
        }
        info!(generation = %worker.id, pid, socket = %worker.address.display(), "subrouter worker ready");
        Ok(worker)
    }

    async fn wait_ready(worker: &Worker, timeout: Duration) -> anyhow::Result<()> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            if let Some(status) = worker.exited() {
                bail!("worker exited before readiness: {status}");
            }
            let last_error = match ready_once(&worker.address).await {
                Ok(()) => return Ok(()),
                Err(error) => error.to_string(),
            };
            if tokio::time::Instant::now() >= deadline {
                bail!(
                    "worker readiness timed out after {}: {}",
                    humantime::format_duration(timeout),
                    last_error
                );
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }

    async fn ready_once(address: &Path) -> anyhow::Result<()> {
        let mut stream =
            tokio::time::timeout(Duration::from_secs(1), UnixStream::connect(address)).await??;
        stream
            .write_all(b"PROXY UNKNOWN\r\nGET /_subrouter/ready HTTP/1.1\r\nHost: subrouter-worker\r\nConnection: close\r\n\r\n")
            .await?;
        let mut response = Vec::new();
        tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response)).await??;
        let status = response
            .split(|byte| *byte == b'\n')
            .next()
            .unwrap_or_default();
        if !status.windows(5).any(|window| window == b" 200 ") {
            bail!(
                "ready check returned {}",
                String::from_utf8_lossy(status).trim()
            );
        }
        Ok(())
    }

    fn prepare_control_socket(path: &Path) -> anyhow::Result<()> {
        let parent = path
            .parent()
            .ok_or_else(|| anyhow!("control socket has no parent"))?;
        fs::create_dir_all(parent)?;
        fs::set_permissions(parent, fs::Permissions::from_mode(0o700))?;
        match fs::symlink_metadata(path) {
            Ok(metadata) if metadata.file_type().is_socket() => fs::remove_file(path)?,
            Ok(_) => bail!(
                "control socket path exists and is not a socket: {}",
                path.display()
            ),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        Ok(())
    }

    fn signal(pid: u32, value: Signal) -> anyhow::Result<()> {
        match kill(Pid::from_raw(i32::try_from(pid)?), value) {
            Ok(()) | Err(Errno::ESRCH) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    async fn control_status(State(supervisor): State<Arc<Supervisor>>) -> Json<serde_json::Value> {
        Json(json!({"active": supervisor.router.active(), "backends": supervisor.router.status()}))
    }

    async fn control_upgrade(State(supervisor): State<Arc<Supervisor>>) -> Response {
        match supervisor.upgrade().await {
            Ok(()) => Json(json!({"active": supervisor.router.active()})).into_response(),
            Err(error) => (StatusCode::BAD_GATEWAY, format!("{error}\n")).into_response(),
        }
    }

    struct SocketCleanup(PathBuf);

    impl Drop for SocketCleanup {
        fn drop(&mut self) {
            let _ = fs::remove_file(&self.0);
        }
    }

    fn parse_duration(value: &str) -> Result<Duration, String> {
        humantime::parse_duration(value).map_err(|error| error.to_string())
    }
}

#[cfg(unix)]
pub use unix::run;

#[cfg(not(unix))]
pub async fn run(_args: &[String]) -> anyhow::Result<()> {
    anyhow::bail!("the stable worker supervisor requires Unix sockets")
}
