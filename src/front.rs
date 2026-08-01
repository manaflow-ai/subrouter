use std::collections::HashMap;
use std::io;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::extract::connect_info::Connected;
use axum::serve::{IncomingStream, Listener};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader, ReadBuf};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Notify;

const MAX_PROXY_PROTOCOL_LINE: usize = 108;

/// The original client address attached to requests. Supervisor workers recover
/// this from the PROXY protocol; directly bound listeners use the TCP peer.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ClientAddr(pub Option<SocketAddr>);

#[derive(Debug)]
pub struct ClientTcpListener(TcpListener);

impl ClientTcpListener {
    pub async fn bind(address: SocketAddr) -> io::Result<Self> {
        TcpListener::bind(address).await.map(Self)
    }

    pub fn from_std(listener: std::net::TcpListener) -> io::Result<Self> {
        listener.set_nonblocking(true)?;
        TcpListener::from_std(listener).map(Self)
    }

    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        self.0.local_addr()
    }
}

impl Listener for ClientTcpListener {
    type Io = TcpStream;
    type Addr = ClientAddr;

    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        loop {
            match self.0.accept().await {
                Ok((stream, address)) => return (stream, ClientAddr(Some(address))),
                Err(error) => {
                    tracing::error!(%error, "TCP accept failed");
                    tokio::time::sleep(Duration::from_secs(1)).await;
                }
            }
        }
    }

    fn local_addr(&self) -> io::Result<Self::Addr> {
        self.0.local_addr().map(|address| ClientAddr(Some(address)))
    }
}

#[cfg(unix)]
#[derive(Debug)]
pub struct ProxyUnixListener(tokio::net::UnixListener);

#[cfg(unix)]
impl ProxyUnixListener {
    pub fn bind(path: &Path) -> io::Result<Self> {
        tokio::net::UnixListener::bind(path).map(Self)
    }

    pub fn from_std(listener: std::os::unix::net::UnixListener) -> io::Result<Self> {
        listener.set_nonblocking(true)?;
        tokio::net::UnixListener::from_std(listener).map(Self)
    }
}

#[cfg(unix)]
impl Listener for ProxyUnixListener {
    type Io = ProxyStream<tokio::net::UnixStream>;
    type Addr = ClientAddr;

    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        loop {
            match self.0.accept().await {
                Ok((stream, _)) => match ProxyStream::read_header(stream, None, None).await {
                    Ok(stream) => {
                        let address = ClientAddr(stream.remote_addr);
                        return (stream, address);
                    }
                    Err(error) => {
                        tracing::warn!(%error, "rejected worker connection without a valid PROXY header")
                    }
                },
                Err(error) => {
                    tracing::error!(%error, "Unix socket accept failed");
                    tokio::time::sleep(Duration::from_secs(1)).await;
                }
            }
        }
    }

    fn local_addr(&self) -> io::Result<Self::Addr> {
        self.0.local_addr().map(|_| ClientAddr(None))
    }
}

impl Connected<IncomingStream<'_, ClientTcpListener>> for ClientAddr {
    fn connect_info(stream: IncomingStream<'_, ClientTcpListener>) -> Self {
        *stream.remote_addr()
    }
}

#[cfg(unix)]
impl Connected<IncomingStream<'_, ProxyUnixListener>> for ClientAddr {
    fn connect_info(stream: IncomingStream<'_, ProxyUnixListener>) -> Self {
        *stream.remote_addr()
    }
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Backend {
    #[serde(default = "default_network")]
    pub network: String,
    pub id: String,
    pub address: String,
}

fn default_network() -> String {
    "tcp".into()
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct BackendStatus {
    pub id: String,
    pub network: String,
    pub address: String,
    pub connections: usize,
    pub active: bool,
}

#[derive(Debug, Error)]
pub enum FrontError {
    #[error("backend id is required")]
    MissingId,
    #[error("backend address is required")]
    MissingAddress,
    #[error("unsupported backend network {0:?}")]
    UnsupportedNetwork(String),
    #[error("backend {id:?} already uses address {address:?}")]
    AddressConflict { id: String, address: String },
    #[error("cannot forget active backend {0:?}")]
    ForgetActive(String),
    #[error("cannot forget backend {id:?} with {connections} connections")]
    ForgetBusy { id: String, connections: usize },
    #[error(transparent)]
    Io(#[from] io::Error),
}

#[derive(Debug)]
struct BackendState {
    backend: Backend,
    connections: usize,
}

#[derive(Debug)]
struct RouterState {
    active: String,
    backends: HashMap<String, BackendState>,
}

#[derive(Clone, Debug)]
pub struct Router {
    state: Arc<Mutex<RouterState>>,
    activity: Arc<Notify>,
    dial_timeout: Duration,
}

impl Router {
    pub fn new(initial: Backend) -> Result<Self, FrontError> {
        let initial = normalize_backend(initial);
        validate_backend(&initial)?;
        let id = initial.id.clone();
        Ok(Self {
            state: Arc::new(Mutex::new(RouterState {
                active: id.clone(),
                backends: HashMap::from([(
                    id,
                    BackendState {
                        backend: initial,
                        connections: 0,
                    },
                )]),
            })),
            activity: Arc::new(Notify::new()),
            dial_timeout: Duration::from_secs(10),
        })
    }

    pub fn switch(&self, backend: Backend) -> Result<(), FrontError> {
        let backend = normalize_backend(backend);
        validate_backend(&backend)?;
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(existing) = state.backends.get(&backend.id) {
            if existing.backend.address != backend.address
                || existing.backend.network != backend.network
            {
                return Err(FrontError::AddressConflict {
                    id: backend.id,
                    address: existing.backend.address.clone(),
                });
            }
            state.active = backend.id;
            return Ok(());
        }
        state.active = backend.id.clone();
        state.backends.insert(
            backend.id.clone(),
            BackendState {
                backend,
                connections: 0,
            },
        );
        Ok(())
    }

    #[must_use]
    pub fn active(&self) -> Backend {
        let state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state.backends[&state.active].backend.clone()
    }

    #[must_use]
    pub fn status(&self) -> Vec<BackendStatus> {
        let state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state
            .backends
            .values()
            .map(|backend| BackendStatus {
                id: backend.backend.id.clone(),
                network: backend.backend.network.clone(),
                address: backend.backend.address.clone(),
                connections: backend.connections,
                active: backend.backend.id == state.active,
            })
            .collect()
    }

    pub async fn wait_idle(&self, id: &str) {
        loop {
            let notified = self.activity.notified();
            let idle = {
                let state = self
                    .state
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner);
                state
                    .backends
                    .get(id)
                    .is_none_or(|backend| backend.connections == 0)
            };
            if idle {
                return;
            }
            notified.await;
        }
    }

    pub async fn wait_all_idle(&self) {
        loop {
            let notified = self.activity.notified();
            let idle = self
                .state
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .backends
                .values()
                .all(|backend| backend.connections == 0);
            if idle {
                return;
            }
            notified.await;
        }
    }

    pub fn forget(&self, id: &str) -> Result<(), FrontError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let Some(backend) = state.backends.get(id) else {
            return Ok(());
        };
        if state.active == id {
            return Err(FrontError::ForgetActive(id.into()));
        }
        if backend.connections != 0 {
            return Err(FrontError::ForgetBusy {
                id: id.into(),
                connections: backend.connections,
            });
        }
        state.backends.remove(id);
        Ok(())
    }

    pub async fn serve(&self, listener: TcpListener) -> Result<(), FrontError> {
        loop {
            let (client, remote) = listener.accept().await?;
            let local = client.local_addr()?;
            let (backend, generation) = self.acquire_active();
            let router = self.clone();
            tokio::spawn(async move {
                let _guard = ConnectionGuard { router, generation };
                let _ =
                    serve_connection(client, remote, local, &backend, _guard.router.dial_timeout)
                        .await;
            });
        }
    }

    fn acquire_active(&self) -> (Backend, String) {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let active = state.active.clone();
        let backend = state
            .backends
            .get_mut(&active)
            .expect("active backend exists");
        backend.connections += 1;
        (backend.backend.clone(), active)
    }

    fn release(&self, generation: &str) {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(backend) = state.backends.get_mut(generation) {
            backend.connections = backend.connections.saturating_sub(1);
        }
        drop(state);
        self.activity.notify_waiters();
    }
}

struct ConnectionGuard {
    router: Router,
    generation: String,
}

impl Drop for ConnectionGuard {
    fn drop(&mut self) {
        self.router.release(&self.generation);
    }
}

async fn serve_connection(
    client: TcpStream,
    remote: SocketAddr,
    local: SocketAddr,
    backend: &Backend,
    timeout: Duration,
) -> io::Result<()> {
    match backend.network.as_str() {
        "tcp" => {
            let mut upstream = tokio::time::timeout(timeout, TcpStream::connect(&backend.address))
                .await
                .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "backend dial timed out"))??;
            write_proxy_protocol_header(&mut upstream, Some(remote), Some(local)).await?;
            proxy_bidirectional(client, upstream).await
        }
        "unix" => serve_unix(client, remote, local, &backend.address, timeout).await,
        _ => Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "unsupported backend network",
        )),
    }
}

#[cfg(unix)]
async fn serve_unix(
    mut client: TcpStream,
    remote: SocketAddr,
    local: SocketAddr,
    address: &str,
    timeout: Duration,
) -> io::Result<()> {
    let mut upstream =
        tokio::time::timeout(timeout, tokio::net::UnixStream::connect(Path::new(address)))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "backend dial timed out"))??;
    write_proxy_protocol_header(&mut upstream, Some(remote), Some(local)).await?;
    let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await?;
    Ok(())
}

#[cfg(not(unix))]
async fn serve_unix(
    _client: TcpStream,
    _remote: SocketAddr,
    _local: SocketAddr,
    _address: &str,
    _timeout: Duration,
) -> io::Result<()> {
    Err(io::Error::new(
        io::ErrorKind::Unsupported,
        "unix sockets are not supported",
    ))
}

async fn proxy_bidirectional(mut client: TcpStream, mut upstream: TcpStream) -> io::Result<()> {
    let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await?;
    Ok(())
}

fn normalize_backend(mut backend: Backend) -> Backend {
    if backend.network.is_empty() {
        backend.network = default_network();
    }
    backend
}

fn validate_backend(backend: &Backend) -> Result<(), FrontError> {
    if backend.id.is_empty() {
        return Err(FrontError::MissingId);
    }
    if backend.address.is_empty() {
        return Err(FrontError::MissingAddress);
    }
    if !matches!(backend.network.as_str(), "tcp" | "unix") {
        return Err(FrontError::UnsupportedNetwork(backend.network.clone()));
    }
    Ok(())
}

pub async fn write_proxy_protocol_header<W: AsyncWrite + Unpin>(
    writer: &mut W,
    source: Option<SocketAddr>,
    destination: Option<SocketAddr>,
) -> io::Result<()> {
    let line = match (source, destination) {
        (Some(source), Some(destination)) if source.is_ipv4() == destination.is_ipv4() => format!(
            "PROXY {} {} {} {} {}\r\n",
            if source.is_ipv4() { "TCP4" } else { "TCP6" },
            source.ip(),
            destination.ip(),
            source.port(),
            destination.port()
        ),
        _ => "PROXY UNKNOWN\r\n".into(),
    };
    writer.write_all(line.as_bytes()).await
}

#[derive(Debug, Error)]
pub enum ProxyProtocolError {
    #[error("read PROXY header: {0}")]
    Read(#[source] io::Error),
    #[error("PROXY header is too long")]
    TooLong,
    #[error("invalid PROXY header")]
    Invalid,
    #[error("invalid PROXY source: {0}")]
    InvalidSource(String),
    #[error("invalid PROXY destination: {0}")]
    InvalidDestination(String),
}

pub struct ProxyStream<S> {
    stream: BufReader<S>,
    pub remote_addr: Option<SocketAddr>,
    pub local_addr: Option<SocketAddr>,
}

impl<S: AsyncRead + AsyncWrite + Unpin> ProxyStream<S> {
    pub async fn read_header(
        stream: S,
        actual_remote: Option<SocketAddr>,
        actual_local: Option<SocketAddr>,
    ) -> Result<Self, ProxyProtocolError> {
        let mut stream = BufReader::with_capacity(MAX_PROXY_PROTOCOL_LINE + 1, stream);
        let mut line = Vec::new();
        tokio::time::timeout(Duration::from_secs(5), stream.read_until(b'\n', &mut line))
            .await
            .map_err(|_| {
                ProxyProtocolError::Read(io::Error::new(io::ErrorKind::TimedOut, "timed out"))
            })?
            .map_err(ProxyProtocolError::Read)?;
        if line.len() > MAX_PROXY_PROTOCOL_LINE {
            return Err(ProxyProtocolError::TooLong);
        }
        let line = std::str::from_utf8(&line).map_err(|_| ProxyProtocolError::Invalid)?;
        let fields: Vec<_> = line.split_ascii_whitespace().collect();
        if fields == ["PROXY", "UNKNOWN"] {
            return Ok(Self {
                stream,
                remote_addr: actual_remote,
                local_addr: actual_local,
            });
        }
        if fields.len() != 6 || fields[0] != "PROXY" || !matches!(fields[1], "TCP4" | "TCP6") {
            return Err(ProxyProtocolError::Invalid);
        }
        let remote_addr =
            parse_proxy_addr(fields[2], fields[4]).map_err(ProxyProtocolError::InvalidSource)?;
        let local_addr = parse_proxy_addr(fields[3], fields[5])
            .map_err(ProxyProtocolError::InvalidDestination)?;
        if (fields[1] == "TCP4") != remote_addr.is_ipv4()
            || remote_addr.is_ipv4() != local_addr.is_ipv4()
        {
            return Err(ProxyProtocolError::Invalid);
        }
        Ok(Self {
            stream,
            remote_addr: Some(remote_addr),
            local_addr: Some(local_addr),
        })
    }
}

fn parse_proxy_addr(host: &str, port: &str) -> Result<SocketAddr, String> {
    let ip = host
        .parse::<std::net::IpAddr>()
        .map_err(|_| format!("invalid IP {host:?}"))?;
    let port = port
        .parse::<u16>()
        .map_err(|_| format!("invalid port {port:?}"))?;
    Ok(SocketAddr::new(ip, port))
}

impl<S: AsyncRead + Unpin> AsyncRead for ProxyStream<S> {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        std::pin::Pin::new(&mut self.stream).poll_read(context, buffer)
    }
}

impl<S: AsyncRead + AsyncWrite + Unpin> AsyncWrite for ProxyStream<S> {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
        buffer: &[u8],
    ) -> std::task::Poll<Result<usize, io::Error>> {
        std::pin::Pin::new(self.stream.get_mut()).poll_write(context, buffer)
    }

    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<(), io::Error>> {
        std::pin::Pin::new(self.stream.get_mut()).poll_flush(context)
    }

    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<(), io::Error>> {
        std::pin::Pin::new(self.stream.get_mut()).poll_shutdown(context)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    #[tokio::test]
    async fn proxy_protocol_restores_addresses_and_payload() {
        let (mut client, server) = tokio::io::duplex(512);
        client
            .write_all(b"PROXY TCP4 192.0.2.10 198.51.100.20 43210 31415\r\nhello\n")
            .await
            .unwrap();
        client.shutdown().await.unwrap();
        let mut server = ProxyStream::read_header(server, None, None).await.unwrap();
        assert_eq!(server.remote_addr.unwrap().to_string(), "192.0.2.10:43210");
        assert_eq!(
            server.local_addr.unwrap().to_string(),
            "198.51.100.20:31415"
        );
        let mut payload = String::new();
        server.read_to_string(&mut payload).await.unwrap();
        assert_eq!(payload, "hello\n");
    }

    #[tokio::test]
    async fn switch_pins_existing_connections() {
        async fn backend(name: &'static str) -> SocketAddr {
            let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            tokio::spawn(async move {
                loop {
                    let Ok((stream, _)) = listener.accept().await else {
                        break;
                    };
                    tokio::spawn(async move {
                        let mut stream = BufReader::new(stream);
                        let mut line = String::new();
                        stream.read_line(&mut line).await.unwrap();
                        loop {
                            line.clear();
                            if stream.read_line(&mut line).await.unwrap() == 0 {
                                break;
                            }
                            let reply = format!("{name}:{}", line);
                            stream.get_mut().write_all(reply.as_bytes()).await.unwrap();
                        }
                    });
                }
            });
            address
        }
        let a = backend("a").await;
        let b = backend("b").await;
        let router = Router::new(Backend {
            network: "tcp".into(),
            id: "a".into(),
            address: a.to_string(),
        })
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn({
            let router = router.clone();
            async move {
                let _ = router.serve(listener).await;
            }
        });
        let mut old = BufReader::new(TcpStream::connect(address).await.unwrap());
        old.get_mut().write_all(b"one\n").await.unwrap();
        let mut line = String::new();
        old.read_line(&mut line).await.unwrap();
        assert_eq!(line, "a:one\n");
        router
            .switch(Backend {
                network: "tcp".into(),
                id: "b".into(),
                address: b.to_string(),
            })
            .unwrap();
        old.get_mut().write_all(b"two\n").await.unwrap();
        line.clear();
        old.read_line(&mut line).await.unwrap();
        assert_eq!(line, "a:two\n");
        let mut new = BufReader::new(TcpStream::connect(address).await.unwrap());
        new.get_mut().write_all(b"three\n").await.unwrap();
        line.clear();
        new.read_line(&mut line).await.unwrap();
        assert_eq!(line, "b:three\n");
    }
}
