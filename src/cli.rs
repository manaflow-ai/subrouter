use std::env;
use std::ffi::OsString;
use std::io::{self, BufRead, IsTerminal, Write};
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use aws_credential_types::provider::ProvideCredentials as _;
use clap::{ArgAction, Parser};
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::{Client, Url};
use serde_json::Value;
use tokio::process::Command;
use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

use crate::account::{Account, Provider};
use crate::accounts::CodexStore;
use crate::agents::{claude, gemini, opencode, pi};
use crate::broker::{self, BrokerClient, CredentialSource};
use crate::front::{ClientAddr, ClientTcpListener};
use crate::proxy::{
    AccountRef, BedrockConfig, BedrockCredentialSource, BedrockQuotaBumper, MultiTenant, Server,
    Upstreams,
};
use crate::selectacct::{LimitWindow, Scheduler, SchedulerRef, Score, score_from_limit_windows};
use crate::{servers, session, storepath, tenant, transcript};

const LOCAL_BASE_URL: &str = "http://127.0.0.1:31415";
const HELP: &str = r"Subrouter routes coding-agent traffic across credential pools.

Service:
  subrouter serve [options]
  subrouter supervise [options] -- [serve options]
  subrouter install-daemon [options]
  subrouter install-systemd [options]
  subrouter install-windows-task [options]

Accounts:
  sr                              Show Codex and Claude usage
  sr add [codex|claude]           Add an account through the provider CLI
  sr add-key                      Add an API-key account
  sr import                       Import the active Codex login
  sr list                         List Codex accounts
  sr switch [account]             Select active credentials and sync agent stores
  sr gui [account]                Switch and restart Codex.app
  sr remove <account>             Remove a stored account
  sr status                       Show provider usage without prompting
  sr pick                         Switch to the recommended account
  sr reset [account]              Redeem a reset credit
  sr usage [days]                 Show API-key spend
  sr trace <account>              Show OAuth refresh breadcrumbs

Shared storage:
  sr setup                        Configure storage and install the daemon
  sr login                        Authenticate with cmux.com
  sr logout                       Revoke this machine's cmux.com session
  sr storage [team|local|legacy]  Show or select credential storage
  sr team <list|current|use>      Manage the selected team
  sr account <command>            Manage team credentials
  sr doctor                       Diagnose the local installation
  sr cleanup                      Remove the local installation

Agents and servers:
  sr codex [args]                 Run Codex through Subrouter
  sr claude [args]                Run Claude through Subrouter
  sr claude-aws [args]            Run Claude through AWS Bedrock
  sr claude-direct [args]         Run Claude directly through Anthropic
  sr gemini [args]                Manage Gemini profiles
  sr server <command>             Manage named Subrouter servers
  sr tenant <command>             Manage tenant keys
  sr daemon <command>             Manage the installed local daemon

OpenAI administration:
  sr admin-keys                   List stored admin keys
  sr add-admin-key                Add an admin key
  sr remove-admin-key <label>     Remove an admin key
  sr attach-project <account>     Attach an API-key account to a project
  sr spend                        Show Bedrock spend tracked by the server

Account commands also work as subrouter <command>. The cx alias is retained.
Run subrouter serve --help for daemon options.
";

#[derive(Parser)]
#[command(name = "subrouter serve", disable_help_subcommand = true)]
struct ServeOptions {
    #[arg(long, default_value = "127.0.0.1:31415")]
    addr: SocketAddr,
    #[arg(long, hide = true)]
    worker_socket: Option<PathBuf>,
    #[arg(long)]
    upstream: Option<Url>,
    #[arg(long, default_value = "https://chatgpt.com/backend-api/codex")]
    codex_upstream: Url,
    #[arg(long, default_value = "https://api.openai.com")]
    api_upstream: Url,
    #[arg(long, default_value = "https://api.anthropic.com")]
    claude_upstream: Url,
    #[arg(long, default_value = "https://api.kimi.com/coding/v1")]
    kimi_upstream: Url,
    #[arg(long, default_value = "https://api.z.ai/api/coding/paas/v4")]
    zai_upstream: Url,
    #[arg(long)]
    sessions: Option<PathBuf>,
    #[arg(long)]
    transcripts: Option<PathBuf>,
    #[arg(long)]
    transcript_gcs_uri: Option<String>,
    #[arg(long, default_value = "5m", value_parser = parse_duration)]
    transcript_gcs_sync_interval: Duration,
    #[arg(long, default_value = "30m", value_parser = parse_duration)]
    transcript_gcs_sync_timeout: Duration,
    #[arg(long, default_value = "0s", value_parser = parse_duration)]
    transcript_local_retention: Duration,
    #[arg(long, default_value = "0")]
    transcript_max_local_bytes: String,
    #[arg(long, alias = "cx-switch-interval", default_value = "10m", value_parser = parse_duration)]
    sr_switch_interval: Duration,
    #[arg(long, default_value = "30s", value_parser = parse_duration)]
    usage_score_ttl: Duration,
    #[arg(long, default_value = "10m", value_parser = parse_duration)]
    shutdown_timeout: Duration,
    #[arg(long, env = "SUBROUTER_ADMIN_TOKEN", hide_env_values = true)]
    admin_token: Option<String>,
    #[arg(long, env = "SUBROUTER_REQUIRE_SESSION_LEASES", action = ArgAction::SetTrue)]
    require_session_leases: bool,
    #[arg(long, default_value_t = 1 << 20)]
    max_body_bytes: usize,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    fetch_usage: bool,
    #[arg(long)]
    multi_tenant: bool,
    #[arg(long)]
    bedrock: bool,
    #[arg(long, default_value = "us-east-1")]
    bedrock_region: String,
    #[arg(long, env = "SUBROUTER_BEDROCK_GATEWAY_TOKEN", hide_env_values = true)]
    bedrock_gateway_token: Option<String>,
    #[arg(long, env = "SUBROUTER_BEDROCK_PROFILES")]
    bedrock_profiles: Option<String>,
    #[arg(long)]
    bedrock_autobump: bool,
    #[arg(long, env = "SUBROUTER_FABLE_BEDROCK_PRIMARY", action = ArgAction::SetTrue)]
    fable_bedrock_primary: bool,
    #[arg(long)]
    cloud_config: Option<PathBuf>,
}

pub async fn main_entry() -> anyhow::Result<()> {
    init_logging();
    let argv: Vec<OsString> = env::args_os().collect();
    let program = argv
        .first()
        .and_then(|value| Path::new(value).file_stem())
        .and_then(|value| value.to_str())
        .unwrap_or("subrouter")
        .to_owned();
    let args: Vec<String> = argv
        .into_iter()
        .skip(1)
        .map(|value| value.to_string_lossy().into_owned())
        .collect();
    run(&program, &args).await
}

fn init_logging() {
    let filter =
        EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("subrouter=info"));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(false)
        .try_init();
}

async fn run(program: &str, args: &[String]) -> anyhow::Result<()> {
    if args
        .first()
        .is_some_and(|value| matches!(value.as_str(), "help" | "-h" | "--help"))
    {
        print!("{HELP}");
        return Ok(());
    }
    if args.is_empty() {
        if matches!(program, "sr" | "cx") {
            return account_command(&[]).await;
        }
        print!("{HELP}");
        return Ok(());
    }
    match args[0].as_str() {
        "serve" => serve(&args[1..]).await,
        "supervise" => crate::supervisor::run(&args[1..]).await,
        "install-daemon" => crate::service::install_launchd(&args[1..]),
        "install-systemd" => crate::service::install_systemd(&args[1..]),
        "install-windows-task" => crate::service::install_windows_task(&args[1..]),
        "accounts" if program == "subrouter" => list_accounts(),
        "codex" => run_codex(&args[1..]).await,
        "cx" => account_command(&args[1..]).await,
        command
            if direct_account_command(command)
                || command.contains('@')
                || program == "sr"
                || program == "cx" =>
        {
            account_command(args).await
        }
        command => bail!("unknown command {command:?}\n\n{HELP}"),
    }
}

fn direct_account_command(command: &str) -> bool {
    matches!(
        command,
        "add"
            | "add-admin-key"
            | "add-key"
            | "add-api-key"
            | "admin-keys"
            | "account"
            | "accounts"
            | "attach-project"
            | "import"
            | "list"
            | "ls"
            | "switch"
            | "use"
            | "remove"
            | "rm"
            | "status"
            | "trace"
            | "breadcrumbs"
            | "why"
            | "claude"
            | "claude-aws"
            | "claude-direct"
            | "cleanup"
            | "cost"
            | "doctor"
            | "g"
            | "gemini"
            | "gui"
            | "gui-switch"
            | "gui-use"
            | "list-admin-keys"
            | "login"
            | "logout"
            | "pick"
            | "remove-admin-key"
            | "reset"
            | "server"
            | "servers"
            | "setup"
            | "spend"
            | "storage"
            | "tenant"
            | "tenants"
            | "team"
            | "usage"
            | "daemon"
    )
}

async fn serve(args: &[String]) -> anyhow::Result<()> {
    let options = ServeOptions::try_parse_from(
        std::iter::once("subrouter serve").chain(args.iter().map(String::as_str)),
    )?;
    if options
        .transcript_gcs_uri
        .as_deref()
        .is_some_and(|value| !value.trim().is_empty())
        && options.transcripts.is_none()
    {
        bail!("--transcripts is required when --transcript-gcs-uri is set");
    }
    let transcript_max_local_bytes =
        transcript::gcs::parse_byte_size(&options.transcript_max_local_bytes)
            .map_err(|error| anyhow!("transcript-max-local-bytes: {error}"))?;
    if options.bedrock {
        warn!(regions = %options.bedrock_region, "Bedrock gateway configuration is enabled");
    }
    if options.multi_tenant {
        info!("strict multi-tenant key validation is enabled");
    }

    let cloud =
        broker::load_config(options.cloud_config.as_deref()).context("load cmux.com config")?;
    if cloud.effective_credential_source() == CredentialSource::Team && !cloud.ready() {
        bail!("team credential storage requires login and a selected team; run 'sr login'");
    }
    if cloud.team_mode_ready() && cloud.local_proxy_token.trim().is_empty() {
        bail!("team credential storage has no local proxy secret; run 'sr setup' to repair it");
    }
    if cloud.team_mode_ready()
        && (options.bedrock
            || env::var("SUBROUTER_CLAUDE_FABLE_API_KEY")
                .is_ok_and(|value| !value.trim().is_empty())
            || options.fable_bedrock_primary)
    {
        bail!(
            "team credential storage cannot use local Bedrock or personal Fable credential fallback"
        );
    }

    let sessions_path = options
        .sessions
        .unwrap_or_else(|| storepath::state_dir().join("sessions.json"));
    let sessions = Arc::new(session::Store::new(&sessions_path)?);
    let codex_store = CodexStore::default();
    let claude_store = claude::Store::default();
    let mut accounts = Vec::new();
    if !cloud.team_mode_ready() {
        accounts.extend(codex_store.list()?);
        match claude_store.list_accounts().await {
            Ok(values) => accounts.extend(values),
            Err(error) => warn!(%error, "Claude accounts skipped"),
        }
    }
    let scheduler = Arc::new(SchedulerRef::new(optimistic_scheduler(&accounts)));
    let mut server = Server::new(Arc::clone(&sessions), Arc::clone(&scheduler))?;
    server.upstreams = Upstreams {
        override_upstream: options.upstream,
        codex: options.codex_upstream,
        api: options.api_upstream,
        claude: options.claude_upstream,
        kimi: options.kimi_upstream,
        zai: options.zai_upstream,
    };
    server.admin_token = options.admin_token.unwrap_or_default().trim().to_owned();
    server.local_proxy_token = if cloud.team_mode_ready() {
        cloud.local_proxy_token.trim().to_owned()
    } else {
        String::new()
    };
    server.require_session_leases =
        options.require_session_leases || env_true("SUBROUTER_REQUIRE_SESSION_LEASES");
    server.forward_session_headers = env_true("SUBROUTER_FORWARD_SESSION_HEADERS");
    server.max_body_bytes = options.max_body_bytes;
    server.usage_score_ttl = if options.fetch_usage {
        options.usage_score_ttl
    } else {
        Duration::ZERO
    };
    server.claude_fable_api_key = env::var("SUBROUTER_CLAUDE_FABLE_API_KEY")
        .unwrap_or_default()
        .trim()
        .to_owned();
    server.fable_bedrock_primary = options.fable_bedrock_primary;
    if options.bedrock {
        server.bedrock = Some(Arc::new(
            load_bedrock_config(
                &options.bedrock_region,
                options.bedrock_profiles.as_deref(),
                options.bedrock_gateway_token.as_deref().unwrap_or_default(),
                options.bedrock_autobump,
                sessions_path
                    .parent()
                    .unwrap_or_else(|| Path::new("."))
                    .join("bedrock-cost.jsonl"),
            )
            .await?,
        ));
    }
    let transcript_dir = options.transcripts.clone();
    server.transcripts = transcript_dir
        .clone()
        .and_then(transcript::Recorder::new)
        .map(Arc::new);

    let mut local_account_ref = None;
    if cloud.team_mode_ready() {
        server.credential_broker = Some(Arc::new(BrokerClient::new(cloud.clone())?));
    } else {
        let account_client = Client::builder().timeout(Duration::from_secs(15)).build()?;
        let reference =
            AccountRef::new(codex_store.clone(), claude_store, accounts, account_client);
        local_account_ref = Some(reference.clone());
        server.account_ref = Some(reference);
    }

    let server = Arc::new(server);
    if let (Some(source_dir), Some(destination)) =
        (transcript_dir.clone(), options.transcript_gcs_uri.clone())
        && let Some(syncer) = transcript::gcs::Syncer::new(transcript::gcs::Config {
            source_dir,
            destination,
            interval: options.transcript_gcs_sync_interval,
            timeout: options.transcript_gcs_sync_timeout,
            local_retention: options.transcript_local_retention,
            max_local_bytes: transcript_max_local_bytes,
            command: None,
        })?
    {
        tokio::spawn(syncer.run());
    }
    if options.fetch_usage {
        refresh_usage_in_background(Arc::clone(&server));
        if let Some(reference) = local_account_ref {
            crate::autoswitch::spawn(
                options.sr_switch_interval,
                reference,
                codex_store,
                Arc::clone(&server.sessions),
                Arc::clone(&server.scheduler),
                storepath::state_dir(),
            );
        }
    } else if !options.sr_switch_interval.is_zero() {
        info!(interval = %humantime::format_duration(options.sr_switch_interval), "sr auto-switch disabled because usage fetching is disabled");
    }
    let lifecycle = Arc::clone(&server.lifecycle);
    let shutdown_timeout = options.shutdown_timeout;
    let registry = Arc::new(tenant::Registry::new(storepath::state_dir()));
    let app = Arc::new(MultiTenant::new(
        server,
        registry,
        transcript_dir,
        options.multi_tenant,
    ))
    .router();
    if let Some(worker_socket) = options.worker_socket {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let _ = std::fs::remove_file(&worker_socket);
            let listener = crate::front::ProxyUnixListener::bind(&worker_socket)
                .with_context(|| format!("listen on {}", worker_socket.display()))?;
            std::fs::set_permissions(&worker_socket, std::fs::Permissions::from_mode(0o600))?;
            let _cleanup = SocketCleanup(worker_socket.clone());
            info!(socket = %worker_socket.display(), cloud_team = %cloud.team_id, "subrouter worker listening");
            axum::serve(
                listener,
                app.into_make_service_with_connect_info::<ClientAddr>(),
            )
            .with_graceful_shutdown(graceful_shutdown(lifecycle, shutdown_timeout))
            .await?;
        }
        #[cfg(not(unix))]
        bail!("--worker-socket is only supported on Unix");
    } else {
        #[cfg(unix)]
        match inherited_listener()? {
            Some(InheritedListener::Tcp(listener)) => {
                info!(addr = %listener.local_addr()?, "using inherited systemd TCP socket");
                axum::serve(
                    listener,
                    app.into_make_service_with_connect_info::<ClientAddr>(),
                )
                .with_graceful_shutdown(graceful_shutdown(lifecycle, shutdown_timeout))
                .await?;
            }
            Some(InheritedListener::Unix(listener)) => {
                info!("using inherited supervisor Unix socket");
                axum::serve(
                    listener,
                    app.into_make_service_with_connect_info::<ClientAddr>(),
                )
                .with_graceful_shutdown(graceful_shutdown(lifecycle, shutdown_timeout))
                .await?;
            }
            None => {
                let listener = ClientTcpListener::bind(options.addr)
                    .await
                    .with_context(|| format!("listen on {}", options.addr))?;
                info!(addr = %options.addr, cloud_team = %cloud.team_id, "subrouter listening");
                axum::serve(
                    listener,
                    app.into_make_service_with_connect_info::<ClientAddr>(),
                )
                .with_graceful_shutdown(graceful_shutdown(lifecycle, shutdown_timeout))
                .await?;
            }
        }
        #[cfg(not(unix))]
        {
            let listener = ClientTcpListener::bind(options.addr)
                .await
                .with_context(|| format!("listen on {}", options.addr))?;
            info!(addr = %options.addr, cloud_team = %cloud.team_id, "subrouter listening");
            axum::serve(
                listener,
                app.into_make_service_with_connect_info::<ClientAddr>(),
            )
            .with_graceful_shutdown(graceful_shutdown(lifecycle, shutdown_timeout))
            .await?;
        }
    }
    Ok(())
}

#[cfg(unix)]
enum InheritedListener {
    Tcp(ClientTcpListener),
    Unix(crate::front::ProxyUnixListener),
}

#[cfg(unix)]
fn inherited_listener() -> anyhow::Result<Option<InheritedListener>> {
    let mut listeners = listenfd::ListenFd::from_env();
    if listeners.len() == 0 {
        return Ok(None);
    }
    if listeners.len() > 1 {
        warn!(
            listeners = listeners.len(),
            "only the first inherited systemd listener is used"
        );
    }
    let tcp_error = match listeners.take_tcp_listener(0) {
        Ok(Some(listener)) => {
            return ClientTcpListener::from_std(listener)
                .map(InheritedListener::Tcp)
                .map(Some)
                .map_err(Into::into);
        }
        Ok(None) => return Ok(None),
        Err(error) => error,
    };
    match listeners.take_unix_listener(0) {
        Ok(Some(listener)) => crate::front::ProxyUnixListener::from_std(listener)
            .map(InheritedListener::Unix)
            .map(Some)
            .map_err(Into::into),
        Ok(None) => Ok(None),
        Err(unix_error) => {
            bail!("inherited listener is neither TCP ({tcp_error}) nor Unix ({unix_error})")
        }
    }
}

#[cfg(unix)]
struct SocketCleanup(PathBuf);

#[cfg(unix)]
impl Drop for SocketCleanup {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

async fn graceful_shutdown(lifecycle: Arc<crate::proxy::Lifecycle>, timeout: Duration) {
    wait_for_shutdown_signal().await;
    lifecycle.drain();
    let deadline = tokio::time::Instant::now() + timeout;
    while lifecycle.active_requests() > 0 && tokio::time::Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

async fn load_bedrock_config(
    regions: &str,
    profiles: Option<&str>,
    gateway_token: &str,
    autobump: bool,
    cost_path: PathBuf,
) -> anyhow::Result<BedrockConfig> {
    let regions = split_list(regions);
    let primary_region = regions
        .first()
        .cloned()
        .ok_or_else(|| anyhow!("bedrock: no AWS regions configured"))?;
    let profiles = profiles
        .map(split_list)
        .filter(|values| !values.is_empty())
        .unwrap_or_else(discover_bedrock_profiles);
    let names = if profiles.is_empty() {
        vec![String::new()]
    } else {
        profiles
    };
    let mut sources = Vec::new();
    for profile in names {
        let mut loader = aws_config::defaults(aws_config::BehaviorVersion::latest())
            .region(aws_config::Region::new(primary_region.clone()));
        if !profile.is_empty() {
            loader = loader.profile_name(profile.clone());
        }
        let sdk = loader.load().await;
        let credentials = sdk.credentials_provider().ok_or_else(|| {
            anyhow!(
                "AWS profile {:?} has no credential provider",
                if profile.is_empty() {
                    "default"
                } else {
                    &profile
                }
            )
        })?;
        credentials.provide_credentials().await.with_context(|| {
            format!(
                "load AWS credentials for {}",
                if profile.is_empty() {
                    "default".into()
                } else {
                    profile.clone()
                }
            )
        })?;
        sources.push(BedrockCredentialSource {
            name: if profile.is_empty() {
                "default".into()
            } else {
                profile
            },
            credentials,
            bumper: autobump.then(|| BedrockQuotaBumper::new(sdk)),
        });
    }
    let config = BedrockConfig::new(regions, sources, gateway_token, cost_path)?;
    info!(
        regions = %config.regions().join(","),
        profiles = %config.source_names().join(","),
        auth = !gateway_token.trim().is_empty(),
        autobump,
        "Bedrock gateway enabled"
    );
    Ok(config)
}

fn split_list(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .collect()
}

fn discover_bedrock_profiles() -> Vec<String> {
    let mut paths = Vec::new();
    if let Some(path) =
        env::var_os("AWS_CONFIG_FILE").filter(|value| !value.to_string_lossy().trim().is_empty())
    {
        paths.push(PathBuf::from(path));
    }
    if let Some(path) = env::var_os("AWS_SHARED_CREDENTIALS_FILE")
        .filter(|value| !value.to_string_lossy().trim().is_empty())
    {
        paths.push(PathBuf::from(path));
    }
    if paths.is_empty()
        && let Some(home) = env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
    {
        paths.extend([
            PathBuf::from(&home).join(".aws/config"),
            PathBuf::from(home).join(".aws/credentials"),
        ]);
    }
    let mut profiles = std::collections::BTreeMap::new();
    for path in paths {
        let Ok(body) = std::fs::read_to_string(path) else {
            continue;
        };
        for line in body.lines() {
            let Some(section) = line
                .trim()
                .strip_prefix('[')
                .and_then(|value| value.strip_suffix(']'))
            else {
                continue;
            };
            let name = section
                .trim()
                .strip_prefix("profile ")
                .unwrap_or(section.trim());
            let Some(suffix) = name.strip_prefix("aw") else {
                continue;
            };
            if !suffix.is_empty()
                && suffix.bytes().all(|byte| byte.is_ascii_digit())
                && let Ok(index) = suffix.parse::<u64>()
            {
                profiles.entry(index).or_insert_with(|| name.to_owned());
            }
        }
    }
    profiles.into_values().collect()
}

fn optimistic_scheduler(accounts: &[Account]) -> Scheduler {
    Scheduler::new(accounts.iter().map(|account| Score {
        account_id: account.id.clone(),
        provider: account.provider,
        headroom: 1.0,
        short_headroom: 1.0,
        fresh: false,
        ..Score::default()
    }))
}

fn refresh_usage_in_background(server: Arc<Server>) {
    tokio::spawn(async move {
        let Some(reference) = &server.account_ref else {
            return;
        };
        let statuses = reference.usage_statuses().await;
        let scores = statuses
            .iter()
            .filter(|status| status.account.error.is_empty())
            .map(|status| {
                let windows: Vec<_> = status
                    .windows
                    .iter()
                    .map(|window| LimitWindow {
                        name: window.name.clone(),
                        used_percent: window.used_percent,
                        limit_window_seconds: window.limit_window_seconds,
                        reset_after_seconds: window.reset_after_seconds,
                        feature: window.feature.clone(),
                    })
                    .collect();
                let mut score = score_from_limit_windows(&status.account.id, 0, &windows);
                score.provider = status.account.provider;
                score.fresh = status.fresh;
                score
            })
            .collect::<Vec<_>>();
        if !scores.is_empty() {
            server.scheduler.set(Scheduler::new(scores));
        }
    });
}

async fn wait_for_shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut terminate = signal(SignalKind::terminate()).expect("install SIGTERM handler");
        let mut retire = signal(SignalKind::user_defined1()).expect("install SIGUSR1 handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = terminate.recv() => {},
            _ = retire.recv() => {},
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}

async fn account_command(args: &[String]) -> anyhow::Result<()> {
    if let Some(result) = crate::cloud::maybe_dispatch(args).await {
        return result;
    }
    if let Some(result) = crate::local_commands::maybe_dispatch(args).await {
        return result;
    }
    let store = CodexStore::default();
    let Some(command) = args.first().map(String::as_str) else {
        return show_status(&store).await;
    };
    match command {
        "add" => {
            let provider = match args.get(1) {
                Some(provider) => provider.clone(),
                None => prompt_provider()?,
            };
            add_account(&store, &provider).await
        }
        "add-key" | "add-api-key" => add_key(&store, &args[1..]),
        "import" => import_active(&store),
        "list" | "ls" => list_store(&store),
        "status" => show_status(&store).await,
        "switch" | "use" => {
            let selector = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr switch <account>"))?;
            switch_account(&store, selector)
        }
        "remove" | "rm" => {
            let selector = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr remove <account>"))?;
            remove_account(&store, selector)
        }
        "trace" | "breadcrumbs" | "why" => {
            let selector = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr trace <account>"))?;
            trace_account(&store, selector)
        }
        "codex" => run_codex(&args[1..]).await,
        "claude" => claude_command(&args[1..]).await,
        "gemini" => gemini_command(&args[1..]),
        "server" | "servers" => server_command(&args[1..]).await,
        "tenant" | "tenants" => tenant_command(&args[1..]).await,
        "daemon" => crate::service::daemon_command(&args[1..]).await,
        "help" | "-h" | "--help" => {
            print!("{HELP}");
            Ok(())
        }
        _ => bail!("unknown account command {command:?}\n\n{HELP}"),
    }
}

pub(crate) async fn add_account(store: &CodexStore, provider: &str) -> anyhow::Result<()> {
    match provider.to_ascii_lowercase().as_str() {
        "codex" | "openai" | "chatgpt" => {
            let previous = store.detect_active_account()?;
            store.sync_active_to_store()?;
            println!("Opening Codex OAuth login...");
            let status = Command::new("codex")
                .arg("login")
                .stdin(Stdio::inherit())
                .stdout(Stdio::inherit())
                .stderr(Stdio::inherit())
                .status()
                .await
                .context("start codex login")?;
            if !status.success() {
                bail!("codex login failed with {status}");
            }
            let (account, existed) = store.import_active()?;
            println!(
                "{} account: {}",
                if existed { "Updated" } else { "Added" },
                account.email
            );
            if let Some(previous) = previous.filter(|previous| previous != &account.email) {
                let selected = store.switch_active(&previous)?;
                sync_compatible_auth(store, &previous)?;
                println!("Restored active account: {}", selected.id);
            }
            Ok(())
        }
        "claude" | "anthropic" => claude_add(None).await,
        _ => bail!("unknown provider {provider:?}; use 'sr add codex' or 'sr add claude'"),
    }
}

pub(crate) fn add_key(store: &CodexStore, args: &[String]) -> anyhow::Result<()> {
    store.sync_active_to_store()?;
    let provider = match flag_value(args, "--provider")
        .unwrap_or_else(|| "codex".into())
        .to_ascii_lowercase()
        .as_str()
    {
        "codex" | "openai" => Provider::Codex,
        "claude" | "anthropic" => Provider::Claude,
        "kimi" | "kimi-for-coding" => Provider::Kimi,
        "zai" | "glm" | "glm-5.2" => Provider::Zai,
        value => {
            bail!("unsupported API-key provider {value:?}, expected codex, claude, kimi, or zai")
        }
    };
    let label = flag_value(args, "--label")
        .map_or_else(|| prompt_line("Label (e.g. work, personal): "), Ok)?;
    let key = flag_value(args, "--key").map_or_else(prompt_secret, Ok)?;
    let (account, existed) = store.add_provider_api_key(provider, &label, &key)?;
    println!(
        "{} {} API key account: {}",
        if existed { "Updated" } else { "Added" },
        provider,
        account.api_key_label()
    );
    Ok(())
}

fn import_active(store: &CodexStore) -> anyhow::Result<()> {
    let (account, existed) = store.import_active()?;
    println!(
        "{} account: {}",
        if existed {
            "Updated existing"
        } else {
            "Imported"
        },
        account.email
    );
    Ok(())
}

fn list_accounts() -> anyhow::Result<()> {
    list_store(&CodexStore::default())
}

fn list_store(store: &CodexStore) -> anyhow::Result<()> {
    let accounts = store.list_stored()?;
    let active = store.detect_active_account()?;
    if accounts.is_empty() {
        println!("No accounts configured. Run 'sr add codex' to add one.");
        return Ok(());
    }
    for account in accounts {
        let marker = if active.as_deref() == Some(&account.email) {
            " *"
        } else {
            ""
        };
        let kind = if account.is_api_key() {
            "API key"
        } else {
            "OAuth"
        };
        println!(
            "{}{}\t{}\t{}",
            account.email,
            marker,
            account.provider_or_default(),
            kind
        );
    }
    Ok(())
}

async fn show_status(store: &CodexStore) -> anyhow::Result<()> {
    let mut accounts = store.list()?;
    accounts.extend(
        claude::Store::default()
            .list_accounts()
            .await
            .unwrap_or_default(),
    );
    if accounts.is_empty() {
        println!("No accounts configured.");
        return Ok(());
    }
    let client = Client::builder().timeout(Duration::from_secs(30)).build()?;
    for account in accounts {
        let result = match account.provider {
            Provider::Codex => crate::accounts::fetch_codex_usage(&client, &account).await,
            Provider::Claude => claude::fetch_fable_usage_windows(&client, &account.token).await,
            Provider::Kimi | Provider::Zai => Ok(Vec::new()),
        };
        match result {
            Ok(windows) if windows.is_empty() => {
                println!("{}\t{}\tavailable", account.provider, account.id)
            }
            Ok(windows) => {
                let limits = windows
                    .iter()
                    .map(|window| format!("{} {:.0}% used", window.name, window.used_percent))
                    .collect::<Vec<_>>()
                    .join(", ");
                println!("{}\t{}\t{}", account.provider, account.id, limits);
            }
            Err(error) => println!("{}\t{}\terror: {}", account.provider, account.id, error),
        }
    }
    Ok(())
}

pub(crate) fn switch_account(store: &CodexStore, selector: &str) -> anyhow::Result<()> {
    let account = store.switch_active(selector)?;
    sync_compatible_auth(store, &account.id)?;
    println!("Switched active account: {}", account.id);
    Ok(())
}

fn sync_compatible_auth(store: &CodexStore, selector: &str) -> anyhow::Result<()> {
    let Some(stored) = store.find_stored(selector)? else {
        return Ok(());
    };
    if stored.is_api_key() {
        return Ok(());
    }
    let path = opencode::sync_codex_account(&stored)?;
    println!("Synced OpenCode auth: {}", path.display());
    let path = pi::sync_codex_account(&stored)?;
    println!("Synced pi auth: {}", path.display());
    Ok(())
}

fn remove_account(store: &CodexStore, selector: &str) -> anyhow::Result<()> {
    match store.remove_stored(selector)? {
        Some(account) => println!("Removed account: {}", account.email),
        None => bail!("no account found matching {selector:?}"),
    }
    Ok(())
}

fn trace_account(store: &CodexStore, selector: &str) -> anyhow::Result<()> {
    let account = store
        .find_stored(selector)?
        .ok_or_else(|| anyhow!("no account found matching {selector:?}"))?;
    println!("OAuth breadcrumbs for {}", account.email);
    if account.breadcrumbs.is_empty() {
        println!("  none");
    }
    for crumb in account.breadcrumbs {
        println!("  {}  {}", crumb.at, crumb.event);
        println!(
            "    source={} reason={} force={}",
            crumb.source, crumb.reason, crumb.force
        );
        if !crumb.source_path.is_empty() {
            println!("    file={}", crumb.source_path);
        }
        if crumb.status_code != 0 || !crumb.provider_code.is_empty() {
            println!(
                "    error_status={} error_code={}",
                crumb.status_code, crumb.provider_code
            );
        }
    }
    Ok(())
}

async fn run_codex(args: &[String]) -> anyhow::Result<()> {
    let base_url = match env::var("SUBROUTER_CODEX_BASE_URL") {
        Ok(value) if !value.trim().is_empty() => value,
        _ => servers::Store::default().select(None)?.map_or_else(
            || format!("{LOCAL_BASE_URL}/v1"),
            |server| server.codex_base_url(),
        ),
    };
    let user = env::var("SUBROUTER_CODEX_USER_EMAIL").unwrap_or_default();
    let account = env::var("SUBROUTER_CODEX_ACCOUNT_ID").unwrap_or_default();
    let model = codex_model_arg(args);
    let routed = codex_args(args, &base_url, &user, &account, &model);
    let mut command =
        Command::new(env::var_os("SUBROUTER_CODEX_BIN").unwrap_or_else(|| OsString::from("codex")));
    command.args(routed);
    command.env("SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter-local-proxy");
    let status = command
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .await
        .context("start Codex")?;
    if !status.success() {
        bail!("Codex exited with {status}");
    }
    Ok(())
}

async fn server_command(args: &[String]) -> anyhow::Result<()> {
    const USAGE: &str = "Usage: sr server <list|add|use|current|clear-default|rename|remove|status|start|stop|restart|install|login|sync|diff> ...";
    let store = servers::Store::default();
    match args.first().map(String::as_str) {
        None | Some("help" | "-h" | "--help") => {
            println!("{USAGE}");
            Ok(())
        }
        Some("list" | "ls") => {
            let file = store.load()?;
            if file.servers.is_empty() {
                println!("No servers configured. Run: sr server add team --url <url>");
                return Ok(());
            }
            for server in file.servers {
                println!(
                    "{}\t{}\t{}{}",
                    server.name,
                    server.url,
                    server.gcp_instance,
                    if server.name == file.default {
                        "\t(default)"
                    } else {
                        ""
                    }
                );
            }
            Ok(())
        }
        Some("add") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server add <name> --url <url> [--default]"))?;
            let url =
                flag_value(&args[2..], "--url").ok_or_else(|| anyhow!("--url is required"))?;
            let admin_set = flag_present(&args[2..], "--admin-token");
            let tenant_set = flag_present(&args[2..], "--tenant-key");
            let existed = store.find(name)?.is_some();
            let server = servers::ServerConfig {
                name: name.clone(),
                url,
                gcp_project: flag_value(&args[2..], "--gcp-project").unwrap_or_default(),
                gcp_zone: flag_value(&args[2..], "--gcp-zone").unwrap_or_default(),
                gcp_instance: flag_value(&args[2..], "--gcp-instance").unwrap_or_default(),
                admin_token: flag_value(&args[2..], "--admin-token").unwrap_or_default(),
                tenant_key: flag_value(&args[2..], "--tenant-key").unwrap_or_default(),
            };
            let make_default = flag_present(&args[2..], "--default");
            store.upsert(server.clone(), make_default, !admin_set, !tenant_set)?;
            println!(
                "{} server: {} ({})",
                if existed { "Saved" } else { "Added" },
                name,
                server.url
            );
            if make_default && !flag_present(&args[2..], "--no-codex-config") {
                let selected = store
                    .find(name)?
                    .ok_or_else(|| anyhow!("server disappeared after save"))?;
                let path = servers::write_codex_config(Some(&selected))?;
                println!("Updated Codex routing: {}", path.display());
            }
            if make_default {
                set_credential_source(CredentialSource::Legacy).await?;
            }
            Ok(())
        }
        Some("use") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server use <name|local>"))?;
            let selected = store.use_server(Some(name))?;
            if !flag_present(&args[2..], "--no-codex-config") {
                let path = servers::write_codex_config(selected.as_ref())?;
                println!("Updated Codex routing: {}", path.display());
            }
            match selected {
                Some(server) => println!("Default server: {} ({})", server.name, server.url),
                None => println!("Default server: local ({LOCAL_BASE_URL})"),
            }
            set_credential_source(if name.eq_ignore_ascii_case("local") {
                CredentialSource::Local
            } else {
                CredentialSource::Legacy
            })
            .await?;
            Ok(())
        }
        Some("current" | "default") => {
            match store.select(None)? {
                Some(server) => println!("{}\t{}", server.name, server.url),
                None => println!("local\t{LOCAL_BASE_URL}"),
            }
            Ok(())
        }
        Some("clear-default" | "unset") => {
            store.use_server(None)?;
            if !flag_present(&args[1..], "--no-codex-config") {
                let path = servers::write_codex_config(None)?;
                println!("Updated Codex routing: {}", path.display());
            }
            println!("Default server: local ({LOCAL_BASE_URL})");
            set_credential_source(CredentialSource::Local).await?;
            Ok(())
        }
        Some("rename" | "mv") => {
            let old = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server rename <old> <new>"))?;
            let new = args
                .get(2)
                .ok_or_else(|| anyhow!("usage: sr server rename <old> <new>"))?;
            store.rename(old, new)?;
            if store.load()?.default == *new {
                let selected = store
                    .find(new)?
                    .ok_or_else(|| anyhow!("renamed server not found"))?;
                let _ = servers::write_codex_config(Some(&selected))?;
            }
            println!("Renamed server: {old} -> {new}");
            Ok(())
        }
        Some("remove" | "rm") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server remove <name>"))?;
            let was_default = store.load()?.default == *name;
            if !store.remove(name)? {
                bail!("server {name:?} not found");
            }
            if was_default {
                let _ = servers::write_codex_config(None)?;
            }
            println!("Removed server: {name}");
            Ok(())
        }
        Some("status") => {
            if let Some(name) = args.get(1) {
                let server = store
                    .find(name)?
                    .ok_or_else(|| anyhow!("server {name:?} not found"))?;
                print_server_status(&server).await
            } else {
                let local = servers::ServerConfig {
                    name: "local".into(),
                    url: LOCAL_BASE_URL.into(),
                    ..servers::ServerConfig::default()
                };
                print_server_health(&local).await?;
                if let Some(selected) = store
                    .select(None)?
                    .filter(|server| server.url != LOCAL_BASE_URL)
                {
                    print_server_health(&selected).await?;
                }
                Ok(())
            }
        }
        Some("start" | "up") => crate::service::daemon_command(&["start".into()]).await,
        Some("stop" | "down") => crate::service::daemon_command(&["stop".into()]).await,
        Some("restart") => crate::service::daemon_command(&["restart".into()]).await,
        Some("install") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server install <name> [--version latest]"))?;
            let server = store
                .find(name)?
                .ok_or_else(|| anyhow!("server {name:?} not found"))?;
            crate::remote::install_server(&server, &args[2..]).await
        }
        Some("login" | "add-account" | "add-auth") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server login <name> [--device-auth]"))?;
            let server = store
                .find(name)?
                .ok_or_else(|| anyhow!("server {name:?} not found"))?;
            crate::remote::login_server(&server, &args[2..]).await
        }
        Some("sync" | "reconcile") => {
            let name = args.get(1).ok_or_else(|| {
                anyhow!("usage: sr server sync <name> [--device-auth] [--all] [--email <email>] [--dry-run] [--yes]")
            })?;
            let server = store
                .find(name)?
                .ok_or_else(|| anyhow!("server {name:?} not found"))?;
            crate::remote::sync_server(&server, &args[2..]).await
        }
        Some("diff") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr server diff <name>"))?;
            let server = store
                .find(name)?
                .ok_or_else(|| anyhow!("server {name:?} not found"))?;
            let mut sync_args = args[2..].to_vec();
            sync_args.push("--dry-run".into());
            crate::remote::sync_server(&server, &sync_args).await
        }
        Some(command) => bail!("unknown server command {command:?}\n{USAGE}"),
    }
}

async fn set_credential_source(source: CredentialSource) -> anyhow::Result<()> {
    let mut config = broker::load_config(None)?;
    config.credential_source = source;
    broker::save_config(None, &config)?;
    crate::service::restart_if_installed().await
}

async fn print_server_health(server: &servers::ServerConfig) -> anyhow::Result<()> {
    let client = Client::builder().timeout(Duration::from_secs(5)).build()?;
    let url = format!("{}/_subrouter/health", server.control_base_url());
    let mut request = client.get(url);
    if !server.admin_token.is_empty() {
        request = request.bearer_auth(&server.admin_token);
    }
    match request.send().await {
        Ok(response) if response.status().is_success() => {
            println!("{}\thealthy\t{}", server.name, server.url)
        }
        Ok(response) => println!(
            "{}\tunhealthy ({})\t{}",
            server.name,
            response.status(),
            server.url
        ),
        Err(error) => println!("{}\tunreachable ({})\t{}", server.name, error, server.url),
    }
    Ok(())
}

async fn print_server_status(server: &servers::ServerConfig) -> anyhow::Result<()> {
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let url = format!("{}/_subrouter/usage-status", server.control_base_url());
    let mut request = client.get(url);
    if !server.admin_token.is_empty() {
        request = request.bearer_auth(&server.admin_token);
    }
    let response = request.send().await?;
    if !response.status().is_success() {
        bail!("server {} returned {}", server.name, response.status());
    }
    let entries: Vec<serde_json::Value> = response.json().await?;
    if entries.is_empty() {
        println!("No accounts configured on {}.", server.name);
    }
    for entry in entries {
        let provider = entry
            .get("provider")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("codex");
        let id = entry
            .get("id")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("unknown");
        let error = entry
            .get("error")
            .and_then(serde_json::Value::as_str)
            .unwrap_or_default();
        let windows = entry
            .get("windows")
            .and_then(serde_json::Value::as_array)
            .map(|windows| {
                windows
                    .iter()
                    .map(|window| {
                        format!(
                            "{} {:.0}% used",
                            window
                                .get("name")
                                .and_then(serde_json::Value::as_str)
                                .unwrap_or("limit"),
                            window
                                .get("used_percent")
                                .and_then(serde_json::Value::as_f64)
                                .unwrap_or_default()
                        )
                    })
                    .collect::<Vec<_>>()
                    .join(", ")
            })
            .unwrap_or_default();
        println!(
            "{provider}\t{id}\t{}",
            if error.is_empty() {
                windows
            } else {
                format!("error: {error}")
            }
        );
    }
    Ok(())
}

fn codex_args(
    args: &[String],
    base_url: &str,
    user: &str,
    account: &str,
    model: &str,
) -> Vec<String> {
    let config = codex_config_args(base_url, user, account, model);
    let Some(first) = args.first() else {
        return config;
    };
    let known = matches!(
        first.as_str(),
        "exec"
            | "e"
            | "review"
            | "login"
            | "logout"
            | "mcp"
            | "plugin"
            | "mcp-server"
            | "app-server"
            | "app"
            | "completion"
            | "sandbox"
            | "debug"
            | "apply"
            | "a"
            | "resume"
            | "fork"
            | "cloud"
            | "exec-server"
            | "features"
            | "help"
    );
    let routed_command = matches!(
        first.as_str(),
        "exec" | "e" | "review" | "resume" | "fork" | "app-server"
    );
    if !known || first.starts_with('-') {
        return config.into_iter().chain(args.iter().cloned()).collect();
    }
    if !routed_command {
        return args.to_vec();
    }
    std::iter::once(first.clone())
        .chain(config)
        .chain(args.iter().skip(1).cloned())
        .collect()
}

fn codex_config_args(base_url: &str, user: &str, account: &str, model: &str) -> Vec<String> {
    if user.is_empty() && account.is_empty() && model.is_empty() {
        return vec!["-c".into(), format!("openai_base_url={base_url:?}")];
    }
    let mut headers = vec![r#""X-Subrouter-Agent"="codex""#.to_owned()];
    if !user.is_empty() {
        headers.push(format!(r#""X-Subrouter-User-Email"={user:?}"#));
    }
    if !account.is_empty() {
        headers.push(format!(r#""X-Subrouter-Account-ID"={account:?}"#));
    }
    if !model.is_empty() {
        headers.push(format!(r#""X-Subrouter-Model"={model:?}"#));
    }
    vec![
        "-c".into(),
        r#"model_provider="subrouter""#.into(),
        "-c".into(),
        r#"model_providers.subrouter.name="Subrouter""#.into(),
        "-c".into(),
        format!("model_providers.subrouter.base_url={base_url:?}"),
        "-c".into(),
        r#"model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY""#.into(),
        "-c".into(),
        r#"model_providers.subrouter.wire_api="responses""#.into(),
        "-c".into(),
        "model_providers.subrouter.supports_websockets=true".into(),
        "-c".into(),
        format!(
            "model_providers.subrouter.http_headers={{{}}}",
            headers.join(",")
        ),
    ]
}

fn codex_model_arg(args: &[String]) -> String {
    for (index, argument) in args.iter().enumerate() {
        if matches!(argument.as_str(), "-m" | "--model") {
            return args.get(index + 1).cloned().unwrap_or_default();
        }
        if let Some(value) = argument
            .strip_prefix("--model=")
            .or_else(|| argument.strip_prefix("-m="))
        {
            return value.to_owned();
        }
    }
    String::new()
}

async fn claude_command(args: &[String]) -> anyhow::Result<()> {
    let store = claude::Store::default();
    match args.first().map(String::as_str) {
        Some("list" | "ls" | "status") => {
            let active = store.active_profile();
            for profile in store.list_profiles() {
                println!(
                    "{}{}\t{}",
                    profile.name,
                    if profile.name == active { " *" } else { "" },
                    store.claude_config_dir(&profile.name).display()
                );
            }
            Ok(())
        }
        Some("add" | "login") => claude_add(args.get(1).cloned()).await,
        Some("switch" | "use") => {
            let name = match args.get(1) {
                Some(name) => name.clone(),
                None => prompt_claude_profile(&store)?,
            };
            let profile = store
                .match_profile(&name)?
                .ok_or_else(|| anyhow!("profile {name:?} not found"))?;
            store.set_active_profile(&profile.name)?;
            println!("Switched Claude profile: {}", profile.name);
            Ok(())
        }
        Some("remove" | "rm") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr claude remove <profile>"))?;
            if !store.remove_profile(name)? {
                bail!("profile {name:?} not found");
            }
            println!("Removed Claude profile: {name}");
            Ok(())
        }
        Some("env") => {
            let name = args
                .get(1)
                .cloned()
                .unwrap_or_else(|| store.active_profile());
            let profile = store
                .match_profile(&name)?
                .ok_or_else(|| anyhow!("profile {name:?} not found"))?;
            println!(
                "export CLAUDE_CONFIG_DIR={}",
                shell_quote(&store.claude_config_dir(&profile.name).display().to_string())
            );
            Ok(())
        }
        Some("pick") => claude_pick(&store).await,
        Some("push" | "upload") => {
            let name = args
                .get(1)
                .cloned()
                .unwrap_or_else(|| store.active_profile());
            if name.is_empty() {
                bail!("usage: sr claude push <profile>");
            }
            claude_push(&name).await
        }
        Some("run") => {
            let (name, launch_args) = match args.get(1) {
                Some(value) if !value.starts_with('-') => (value.clone(), &args[2..]),
                _ => (store.active_profile(), &args[1..]),
            };
            run_claude_profile(&store, &name, launch_args).await
        }
        Some("help" | "-h" | "--help") => {
            println!("Usage: sr claude [list|add|switch|remove|env|pick|push|run] ...");
            Ok(())
        }
        None => run_claude_profile(&store, &store.active_profile(), &[]).await,
        Some(command) if command.starts_with('-') => {
            run_claude_profile(&store, &store.active_profile(), args).await
        }
        Some(command) if store.find_profile(command).is_some() => {
            run_claude_profile(&store, command, &args[1..]).await
        }
        Some(command) => bail!("unknown Claude command {command:?}"),
    }
}

async fn claude_add(name: Option<String>) -> anyhow::Result<()> {
    let store = claude::Store::default();
    let (instance, dir) = store.create_temp_instance()?;
    let claude_bin =
        claude::detect_cli().ok_or_else(|| anyhow!("Claude CLI was not found on PATH"))?;
    println!("Opening Claude login...");
    let result = Command::new(&claude_bin)
        .args(["auth", "login"])
        .env("CLAUDE_CONFIG_DIR", &instance)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .await;
    match result {
        Ok(status) if status.success() => {}
        Ok(status) => {
            store.cleanup_instance(&dir).ok();
            bail!("Claude login failed with {status}");
        }
        Err(error) => {
            store.cleanup_instance(&dir).ok();
            return Err(error.into());
        }
    }
    let auth = claude::auth_status_for_path(&claude_bin, &instance).await;
    let profile_name = name
        .or_else(|| {
            auth.as_ref()
                .map(|status| status.email.clone())
                .filter(|value| !value.is_empty())
        })
        .unwrap_or_else(|| format!("claude-{}", chrono::Utc::now().format("%Y%m%d%H%M%S")));
    claude::validate_profile_name_allow_email(&profile_name)?;
    store.register_profile(&profile_name, &dir)?;
    store.set_active_profile(&profile_name)?;
    println!("Added Claude profile: {profile_name}");
    claude_push_after_add(&profile_name).await
}

fn prompt_claude_profile(store: &claude::Store) -> anyhow::Result<String> {
    let profiles = store.list_profiles();
    if profiles.is_empty() {
        bail!("no Claude profiles configured. Run 'sr claude add' first");
    }
    for (index, profile) in profiles.iter().enumerate() {
        println!("  {}) {}", index + 1, profile.name);
    }
    let answer = prompt_line("Switch to (#): ")?;
    if let Some(index) = answer
        .parse::<usize>()
        .ok()
        .filter(|index| *index > 0 && *index <= profiles.len())
    {
        return Ok(profiles[index - 1].name.clone());
    }
    Ok(answer)
}

async fn claude_pick(store: &claude::Store) -> anyhow::Result<()> {
    let profiles = store.list_profiles();
    if profiles.is_empty() {
        bail!("no Claude profiles configured. Run 'sr claude add' first");
    }
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let mut candidates = Vec::new();
    for profile in profiles {
        let (account, _) = match store.refresh_credential_if_expired(&client, &profile).await {
            Ok(value) => value,
            Err(error) => {
                warn!(profile = %profile.name, %error, "Claude profile skipped during pick");
                continue;
            }
        };
        let windows = claude::fetch_fable_usage_windows(&client, &account.token).await?;
        let limits = windows
            .iter()
            .map(|window| LimitWindow {
                name: window.name.clone(),
                used_percent: window.used_percent,
                limit_window_seconds: window.limit_window_seconds,
                reset_after_seconds: window.reset_after_seconds,
                feature: window.feature.clone(),
            })
            .collect::<Vec<_>>();
        let score = score_from_limit_windows(&profile.name, 0, &limits);
        if !score.exhausted() {
            candidates.push((profile.name, score.headroom.min(score.short_headroom)));
        }
    }
    candidates.sort_by(|left, right| {
        right
            .1
            .total_cmp(&left.1)
            .then_with(|| left.0.cmp(&right.0))
    });
    let target = candidates
        .first()
        .ok_or_else(|| anyhow!("no Claude profile has quota for a new session"))?;
    if store.active_profile() == target.0 {
        println!("Already using recommended Claude profile: {}", target.0);
        return Ok(());
    }
    store.set_active_profile(&target.0)?;
    println!("Picked recommended Claude profile: {}", target.0);
    Ok(())
}

async fn claude_push_after_add(name: &str) -> anyhow::Result<()> {
    match broker::load_config(None)?.effective_credential_source() {
        CredentialSource::Team => {
            crate::cloud::account_import(&["--only".into(), format!("claude:{name}")]).await
        }
        CredentialSource::Local => Ok(()),
        CredentialSource::Legacy | CredentialSource::Unspecified => {
            let Some(server) = servers::Store::default().select(None)? else {
                return Ok(());
            };
            crate::remote::upload_claude_profile(&server, name).await
        }
    }
}

async fn claude_push(name: &str) -> anyhow::Result<()> {
    match broker::load_config(None)?.effective_credential_source() {
        CredentialSource::Team => {
            crate::cloud::account_import(&["--only".into(), format!("claude:{name}")]).await
        }
        CredentialSource::Local => {
            println!("Credential storage is local; profile {name} already stays on this machine.");
            Ok(())
        }
        CredentialSource::Legacy | CredentialSource::Unspecified => {
            let server = servers::Store::default().select(None)?.ok_or_else(|| {
                anyhow!("no default Subrouter server configured; run 'sr server use <name>'")
            })?;
            crate::remote::upload_claude_profile(&server, name).await
        }
    }
}

async fn run_claude_profile(
    store: &claude::Store,
    name: &str,
    args: &[String],
) -> anyhow::Result<()> {
    let config_dir = if name.is_empty() {
        None
    } else {
        let profile = store
            .match_profile(name)?
            .ok_or_else(|| anyhow!("profile {name:?} not found"))?;
        store.set_active_profile(&profile.name)?;
        if let Some(session_id) = claude::resume_session_id(args)
            && let Some(source) = store.migrate_session(&profile.name, &session_id)?
        {
            eprintln!("Copied session {session_id} from profile {source:?}.");
        }
        Some(store.claude_config_dir(&profile.name))
    };
    let cloud = broker::load_config(None)?;
    match cloud.effective_credential_source() {
        CredentialSource::Team if !cloud.ready() => {
            bail!("team credential storage requires login and a selected team; run 'sr login'")
        }
        CredentialSource::Team | CredentialSource::Local => {
            ensure_local_proxy().await?;
            run_claude_at(
                config_dir.as_deref(),
                args,
                Some(
                    if cloud.effective_credential_source() == CredentialSource::Team {
                        cloud.local_proxy_token.as_str()
                    } else {
                        "subrouter"
                    },
                ),
            )
            .await
        }
        CredentialSource::Legacy | CredentialSource::Unspecified => {
            run_claude_at(config_dir.as_deref(), args, None).await
        }
    }
}

async fn ensure_local_proxy() -> anyhow::Result<()> {
    let client = Client::builder().timeout(Duration::from_secs(2)).build()?;
    let healthy = client
        .get(format!("{LOCAL_BASE_URL}/_subrouter/health"))
        .send()
        .await
        .is_ok_and(|response| response.status().is_success());
    if healthy {
        return Ok(());
    }
    if crate::service::daemon_installed() {
        crate::service::daemon_command(&["start".into()]).await?;
        let healthy = client
            .get(format!("{LOCAL_BASE_URL}/_subrouter/health"))
            .send()
            .await
            .is_ok_and(|response| response.status().is_success());
        if healthy {
            return Ok(());
        }
    }
    bail!("local proxy is unavailable; run 'sr doctor'")
}

async fn run_claude_at(
    config_dir: Option<&Path>,
    args: &[String],
    proxy_token: Option<&str>,
) -> anyhow::Result<()> {
    let claude_bin =
        claude::detect_cli().ok_or_else(|| anyhow!("Claude CLI was not found on PATH"))?;
    let mut command = Command::new(claude_bin);
    command
        .args(args)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    if let Some(config_dir) = config_dir {
        command.env("CLAUDE_CONFIG_DIR", config_dir);
    }
    if let Some(proxy_token) = proxy_token {
        for key in [
            "ANTHROPIC_API_KEY",
            "CLAUDE_CODE_USE_BEDROCK",
            "ANTHROPIC_BEDROCK_BASE_URL",
            "CLAUDE_CODE_USE_VERTEX",
            "ANTHROPIC_VERTEX_BASE_URL",
            "CLAUDE_CODE_USE_MANTLE",
            "ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
        ] {
            command.env_remove(key);
        }
        command
            .env("ANTHROPIC_BASE_URL", LOCAL_BASE_URL)
            .env("ANTHROPIC_AUTH_TOKEN", proxy_token);
    }
    let status = command.status().await?;
    if !status.success() {
        bail!("Claude exited with {status}");
    }
    Ok(())
}

fn gemini_command(args: &[String]) -> anyhow::Result<()> {
    let store = gemini::Store::default();
    match args.first().map(String::as_str) {
        None | Some("list" | "ls") => {
            let active = store.active_profile();
            for profile in store.list_profiles() {
                println!(
                    "{}{}\t{}",
                    profile.name,
                    if profile.name == active { " *" } else { "" },
                    profile.dir
                );
            }
            Ok(())
        }
        Some("switch" | "use") => {
            let name = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr gemini switch <profile>"))?;
            store.set_active_profile(name)?;
            println!("Switched Gemini profile: {name}");
            Ok(())
        }
        Some(command) => bail!("unknown Gemini command {command:?}"),
    }
}

async fn tenant_command(args: &[String]) -> anyhow::Result<()> {
    const USAGE: &str = "Usage:\n  sr tenant create <name> [--server <name>]\n  sr tenant list [--server <name>]\n  sr tenant key create <tenant> [--server <name>]\n  sr tenant key revoke <tenant> <key-prefix> [--server <name>]";
    let (args, server) = tenant_target(args)?;
    let registry = tenant::Registry::new(storepath::state_dir());
    match args.first().map(String::as_str) {
        None | Some("help" | "-h" | "--help") => {
            println!("{USAGE}");
            Ok(())
        }
        Some("create") => {
            let name = args.get(1).ok_or_else(|| anyhow!("{USAGE}"))?;
            if args.len() != 2 {
                bail!("{USAGE}");
            }
            if let Some(server) = server {
                let value = tenant_admin_request(
                    &server,
                    reqwest::Method::POST,
                    "/_subrouter/tenants",
                    Some(serde_json::json!({"name": name})),
                )
                .await?;
                let created = value.get("tenant").unwrap_or(&Value::Null);
                let id = created
                    .get("id")
                    .and_then(Value::as_str)
                    .unwrap_or_default();
                let display = created.get("name").and_then(Value::as_str).unwrap_or(name);
                let key = value.get("key").and_then(Value::as_str).unwrap_or_default();
                println!("Created tenant {display} (id {id}) on {}", server.name);
                println!("Tenant key (shown once, store it now): {key}");
                println!("Point clients at <server-url>/t/<tenant-key>");
                return Ok(());
            }
            let (created, key) = registry.create(name)?;
            println!(
                "Created tenant {} (id {}) on local",
                created.name, created.id
            );
            println!("Tenant key (shown once, store it now): {key}");
            println!("Point clients at <server-url>/t/<tenant-key>");
            Ok(())
        }
        Some("list" | "ls") => {
            if args.len() != 1 {
                bail!("{USAGE}");
            }
            if let Some(server) = server {
                let values = tenant_admin_request(
                    &server,
                    reqwest::Method::GET,
                    "/_subrouter/tenants",
                    None,
                )
                .await?;
                let tenants = values.as_array().cloned().unwrap_or_default();
                if tenants.is_empty() {
                    println!("No tenants on {}.", server.name);
                }
                for value in tenants {
                    print_tenant_value(&value);
                }
                return Ok(());
            }
            let values = registry.list()?;
            if values.is_empty() {
                println!("No tenants on local.");
            }
            for value in values {
                let prefixes = value
                    .keys
                    .iter()
                    .map(|key| format!("{}…", key.prefix))
                    .collect::<Vec<_>>()
                    .join(", ");
                println!("{}\t{}\tkeys: {prefixes}", value.id, value.name);
            }
            Ok(())
        }
        Some("key" | "keys")
            if args
                .get(1)
                .is_some_and(|value| matches!(value.as_str(), "create" | "add")) =>
        {
            let reference = args.get(2).ok_or_else(|| anyhow!("{USAGE}"))?;
            if args.len() != 3 {
                bail!("{USAGE}");
            }
            if let Some(server) = server {
                let (id, name) = resolve_remote_tenant(&server, reference).await?;
                let value = tenant_admin_request(
                    &server,
                    reqwest::Method::POST,
                    &format!("/_subrouter/tenants/{id}/keys"),
                    None,
                )
                .await?;
                let key = value.get("key").and_then(Value::as_str).unwrap_or_default();
                println!("New key for tenant {name} (id {id}) on {}", server.name);
                println!("Tenant key (shown once, store it now): {key}");
                return Ok(());
            }
            let found = registry
                .find(reference)?
                .ok_or_else(|| anyhow!("tenant {reference:?} not found"))?;
            let (_, key) = registry.create_key(&found.id)?;
            println!(
                "New key for tenant {} (id {}) on local",
                found.name, found.id
            );
            println!("Tenant key (shown once, store it now): {key}");
            Ok(())
        }
        Some("key" | "keys")
            if args
                .get(1)
                .is_some_and(|value| matches!(value.as_str(), "revoke" | "remove" | "rm")) =>
        {
            let reference = args.get(2).ok_or_else(|| anyhow!("{USAGE}"))?;
            let key = args.get(3).ok_or_else(|| anyhow!("{USAGE}"))?;
            if args.len() != 4 {
                bail!("{USAGE}");
            }
            if let Some(server) = server {
                let (id, name) = resolve_remote_tenant(&server, reference).await?;
                let encoded_key = utf8_percent_encode(key, NON_ALPHANUMERIC);
                let value = tenant_admin_request(
                    &server,
                    reqwest::Method::DELETE,
                    &format!("/_subrouter/tenants/{id}/keys/{encoded_key}"),
                    None,
                )
                .await?;
                let removed = value
                    .get("revoked")
                    .and_then(Value::as_u64)
                    .unwrap_or_default();
                println!(
                    "Revoked {removed} key(s) matching {} on tenant {name} ({})",
                    tenant::display_key_ref(key),
                    server.name
                );
                return Ok(());
            }
            let found = registry
                .find(reference)?
                .ok_or_else(|| anyhow!("tenant {reference:?} not found"))?;
            let removed = registry.revoke_key(&found.id, key)?;
            println!(
                "Revoked {removed} key(s) matching {} on tenant {} (local)",
                tenant::display_key_ref(key),
                found.name
            );
            Ok(())
        }
        Some(command) => bail!("unknown tenant command {command:?}\n{USAGE}"),
    }
}

fn tenant_target(args: &[String]) -> anyhow::Result<(Vec<String>, Option<servers::ServerConfig>)> {
    let mut positional = Vec::new();
    let mut server_name = None;
    let mut index = 0;
    while index < args.len() {
        if args[index] == "--server" {
            index += 1;
            server_name = Some(
                args.get(index)
                    .ok_or_else(|| anyhow!("--server requires a value"))?
                    .clone(),
            );
        } else if let Some(value) = args[index].strip_prefix("--server=") {
            server_name = Some(value.to_owned());
        } else {
            positional.push(args[index].clone());
        }
        index += 1;
    }
    let store = servers::Store::default();
    let server = match server_name.as_deref() {
        Some("local" | "localhost") => None,
        Some(name) => Some(
            store
                .find(name)?
                .ok_or_else(|| anyhow!("server {name:?} not found"))?,
        ),
        None => store.select(None)?,
    };
    Ok((positional, server))
}

async fn tenant_admin_request(
    server: &servers::ServerConfig,
    method: reqwest::Method,
    path: &str,
    body: Option<Value>,
) -> anyhow::Result<Value> {
    let client = Client::builder().timeout(Duration::from_secs(15)).build()?;
    let mut request = client.request(
        method,
        format!("{}{path}", servers::proxy_root(&server.url)),
    );
    if !server.admin_token.is_empty() {
        request = request.bearer_auth(&server.admin_token);
    }
    if let Some(body) = body {
        request = request.json(&body);
    }
    let response = request.send().await?;
    let status = response.status();
    let bytes = response.bytes().await?;
    if !status.is_success() {
        let message = String::from_utf8_lossy(&bytes[..bytes.len().min(4096)]);
        bail!("tenant admin request failed: {status}: {}", message.trim());
    }
    if bytes.is_empty() {
        return Ok(Value::Null);
    }
    serde_json::from_slice(&bytes).context("decode tenant admin response")
}

async fn resolve_remote_tenant(
    server: &servers::ServerConfig,
    reference: &str,
) -> anyhow::Result<(String, String)> {
    let values =
        tenant_admin_request(server, reqwest::Method::GET, "/_subrouter/tenants", None).await?;
    values
        .as_array()
        .into_iter()
        .flatten()
        .find_map(|value| {
            let id = value.get("id")?.as_str()?;
            let name = value.get("name")?.as_str()?;
            (id == reference || name.eq_ignore_ascii_case(reference))
                .then(|| (id.to_owned(), name.to_owned()))
        })
        .ok_or_else(|| anyhow!("tenant {reference:?} not found on server {}", server.name))
}

fn print_tenant_value(value: &Value) {
    let id = value.get("id").and_then(Value::as_str).unwrap_or_default();
    let name = value
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let prefixes = value
        .get("keys")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|key| key.get("prefix").and_then(Value::as_str))
        .map(|prefix| format!("{prefix}…"))
        .collect::<Vec<_>>()
        .join(", ");
    println!("{id}\t{name}\tkeys: {prefixes}");
}

fn prompt_line(prompt: &str) -> anyhow::Result<String> {
    print!("{prompt}");
    io::stdout().flush()?;
    let mut value = String::new();
    io::stdin().lock().read_line(&mut value)?;
    let value = value.trim().to_owned();
    if value.is_empty() {
        bail!("value is required");
    }
    Ok(value)
}

fn prompt_secret() -> anyhow::Result<String> {
    if io::stdin().is_terminal() {
        let value = rpassword::prompt_password("API key (sk-...): ")?;
        if value.trim().is_empty() {
            bail!("API key is required");
        }
        Ok(value)
    } else {
        prompt_line("API key (sk-...): ")
    }
}

fn prompt_provider() -> anyhow::Result<String> {
    if !io::stdin().is_terminal() {
        bail!("no provider given; run 'sr add codex' or 'sr add claude'");
    }
    print!(
        "Which account do you want to add?\n\n  1) Codex   (ChatGPT subscription or API key)\n  2) Claude  (Anthropic subscription or API key)\n\nChoice [1]: "
    );
    io::stdout().flush()?;
    let mut value = String::new();
    io::stdin().lock().read_line(&mut value)?;
    match value.trim().to_ascii_lowercase().as_str() {
        "" | "1" | "codex" => Ok("codex".into()),
        "2" | "claude" => Ok("claude".into()),
        value => bail!("unrecognized choice {value:?}; run 'sr add codex' or 'sr add claude'"),
    }
}

fn flag_value(args: &[String], name: &str) -> Option<String> {
    args.iter().enumerate().find_map(|(index, value)| {
        value
            .strip_prefix(&format!("{name}="))
            .map(str::to_owned)
            .or_else(|| {
                (value == name)
                    .then(|| args.get(index + 1).cloned())
                    .flatten()
            })
    })
}

fn flag_present(args: &[String], name: &str) -> bool {
    args.iter()
        .any(|value| value == name || value.starts_with(&format!("{name}=")))
}

fn parse_duration(value: &str) -> Result<Duration, String> {
    humantime::parse_duration(value).map_err(|error| error.to_string())
}

fn env_true(name: &str) -> bool {
    env::var(name).is_ok_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes" | "on"
        )
    })
}

fn shell_quote(value: &str) -> String {
    if value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'.' | b'_' | b'-'))
    {
        value.into()
    } else {
        format!("'{}'", value.replace("'", "'\\''"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codex_routing_args_preserve_command_position() {
        let args = vec!["exec".into(), "hello".into()];
        let routed = codex_args(&args, "http://localhost:31415/v1", "", "", "");
        assert_eq!(routed[0], "exec");
        assert_eq!(routed[1], "-c");
        assert_eq!(routed.last().unwrap(), "hello");
    }

    #[test]
    fn utility_codex_commands_are_not_routed() {
        let args = vec!["login".into(), "--device-auth".into()];
        assert_eq!(
            codex_args(&args, "http://localhost/v1", "x@example.com", "", ""),
            args
        );
    }

    #[test]
    fn duration_parser_supports_go_style_units() {
        assert_eq!(parse_duration("10m").unwrap(), Duration::from_secs(600));
        assert_eq!(parse_duration("0s").unwrap(), Duration::ZERO);
    }
}
