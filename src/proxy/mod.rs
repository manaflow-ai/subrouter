mod bedrock;
mod catalog;
mod leases;
mod multitenant;

use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail};
use async_trait::async_trait;
use axum::Router;
use axum::body::{Body, to_bytes};
use axum::extract::ws::{Message as AxumMessage, WebSocket, WebSocketUpgrade};
use axum::extract::{ConnectInfo, FromRequestParts, State};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::any;
use bytes::Bytes;
use chrono::{DateTime, SecondsFormat, Utc};
use futures_util::{SinkExt, Stream, StreamExt};
use http::header::{HeaderName, HeaderValue};
use http::{HeaderMap, Method, Request, StatusCode, Uri};
use reqwest::{Client, Url};
use serde::Serialize;
use serde_json::{Map, Value, json};
use sha2::{Digest as _, Sha256};
use subtle::ConstantTimeEq;
use tokio::sync::Semaphore;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::{Error as WebSocketError, Message as TungsteniteMessage};
use tracing::{debug, error, info, warn};

use crate::account::{Account, AuthMode, Provider};
use crate::accounts::{self, CodexStore, UsageWindow};
use crate::agents::claude;
use crate::broker::{self, BrokerClient};
use crate::front::ClientAddr;
use crate::selectacct::{self, Scheduler, SchedulerRef};
use crate::session;
use crate::transcript::{self, Recorder};

pub use bedrock::{
    BedrockConfig, BedrockCredentialSource, BedrockQuotaBumper, CostSummary as BedrockCostSummary,
};
pub use catalog::{AggregateResult, BufferedResponse, SingleFlight, coalescable_path, flight_key};
pub use leases::{
    Lease, LeaseError, LeaseRequest as SessionLeaseRequest, LeaseResponse, LeaseStore,
};
pub use multitenant::MultiTenant;

const DEFAULT_CODEX_UPSTREAM: &str = "https://chatgpt.com/backend-api/codex";
const DEFAULT_API_UPSTREAM: &str = "https://api.openai.com";
const DEFAULT_CLAUDE_UPSTREAM: &str = "https://api.anthropic.com";
const DEFAULT_KIMI_UPSTREAM: &str = "https://api.kimi.com/coding/v1";
const DEFAULT_ZAI_UPSTREAM: &str = "https://api.z.ai/api/coding/paas/v4";
const MAX_PROXY_BODY_BYTES: usize = 128 << 20;
const MAX_REPLAY_ATTEMPTS: usize = 6;
const USAGE_INSPECT_MAX_BYTES: usize = 1 << 20;
const CLAUDE_OAUTH_BETA_HEADER: &str = "oauth-2025-04-20";
const CREDENTIAL_EXHAUSTION_TTL: Duration = Duration::from_secs(60 * 60);

#[derive(Clone, Debug)]
pub struct Upstreams {
    pub override_upstream: Option<Url>,
    pub codex: Url,
    pub api: Url,
    pub claude: Url,
    pub kimi: Url,
    pub zai: Url,
}

impl Default for Upstreams {
    fn default() -> Self {
        Self {
            override_upstream: None,
            codex: Url::parse(DEFAULT_CODEX_UPSTREAM).expect("static Codex upstream"),
            api: Url::parse(DEFAULT_API_UPSTREAM).expect("static OpenAI upstream"),
            claude: Url::parse(DEFAULT_CLAUDE_UPSTREAM).expect("static Claude upstream"),
            kimi: Url::parse(DEFAULT_KIMI_UPSTREAM).expect("static Kimi upstream"),
            zai: Url::parse(DEFAULT_ZAI_UPSTREAM).expect("static ZAI upstream"),
        }
    }
}

#[derive(Debug)]
pub struct ActiveSessions {
    counts: std::sync::Mutex<HashMap<String, usize>>,
}

impl Default for ActiveSessions {
    fn default() -> Self {
        Self {
            counts: std::sync::Mutex::new(HashMap::new()),
        }
    }
}

impl ActiveSessions {
    fn begin(self: &Arc<Self>, agent: &str, session_id: &str) -> Option<ActiveGuard> {
        if session_id.is_empty() {
            return None;
        }
        let key = session::scoped_session_key(agent, session_id);
        *self
            .counts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .entry(key.clone())
            .or_default() += 1;
        Some(ActiveGuard {
            sessions: Arc::clone(self),
            key,
        })
    }

    #[must_use]
    pub fn active(&self, agent: &str, session_id: &str) -> bool {
        self.counts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(&session::scoped_session_key(agent, session_id))
            .is_some_and(|count| *count > 0)
    }
}

struct ActiveGuard {
    sessions: Arc<ActiveSessions>,
    key: String,
}

impl Drop for ActiveGuard {
    fn drop(&mut self) {
        let mut counts = self
            .sessions
            .counts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if counts.get(&self.key).is_some_and(|count| *count <= 1) {
            counts.remove(&self.key);
        } else if let Some(count) = counts.get_mut(&self.key) {
            *count -= 1;
        }
    }
}

#[derive(Debug)]
pub struct Lifecycle {
    started_at: DateTime<Utc>,
    draining: AtomicBool,
    active: AtomicI64,
}

impl Default for Lifecycle {
    fn default() -> Self {
        Self {
            started_at: Utc::now(),
            draining: AtomicBool::new(false),
            active: AtomicI64::new(0),
        }
    }
}

impl Lifecycle {
    fn begin(self: &Arc<Self>) -> LifecycleGuard {
        self.active.fetch_add(1, Ordering::Relaxed);
        LifecycleGuard(Arc::clone(self))
    }

    pub fn drain(&self) {
        self.draining.store(true, Ordering::Release);
    }

    #[must_use]
    pub fn draining(&self) -> bool {
        self.draining.load(Ordering::Acquire)
    }

    #[must_use]
    pub fn active_requests(&self) -> i64 {
        self.active.load(Ordering::Relaxed)
    }

    #[must_use]
    pub fn status(&self, ok: bool) -> Value {
        json!({
            "ok": ok,
            "draining": self.draining(),
            "active_proxy_requests": self.active_requests(),
            "started_at": self.started_at.to_rfc3339_opts(SecondsFormat::Secs, true),
        })
    }
}

struct LifecycleGuard(Arc<Lifecycle>);

impl Drop for LifecycleGuard {
    fn drop(&mut self) {
        self.0.active.fetch_sub(1, Ordering::Relaxed);
    }
}

#[derive(Default)]
pub struct StreamDropStats {
    client: AtomicU64,
    proxy: AtomicU64,
    upstream: AtomicU64,
    unknown: AtomicU64,
    since_unix: AtomicI64,
    last_proxy_unix: AtomicI64,
}

#[derive(Serialize)]
struct StreamDropSnapshot {
    client: u64,
    proxy: u64,
    upstream: u64,
    unknown: u64,
    total: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    since: String,
    #[serde(rename = "last_proxy_drop", skip_serializing_if = "String::is_empty")]
    last_proxy: String,
}

impl StreamDropStats {
    pub fn observe(&self, canceled_by: &str) {
        let now = Utc::now().timestamp();
        let _ = self
            .since_unix
            .compare_exchange(0, now, Ordering::Relaxed, Ordering::Relaxed);
        match canceled_by {
            "client" => self.client.fetch_add(1, Ordering::Relaxed),
            "proxy" => {
                self.last_proxy_unix.store(now, Ordering::Relaxed);
                self.proxy.fetch_add(1, Ordering::Relaxed)
            }
            "upstream" => self.upstream.fetch_add(1, Ordering::Relaxed),
            _ => self.unknown.fetch_add(1, Ordering::Relaxed),
        };
    }

    fn snapshot(&self) -> StreamDropSnapshot {
        let client = self.client.load(Ordering::Relaxed);
        let proxy = self.proxy.load(Ordering::Relaxed);
        let upstream = self.upstream.load(Ordering::Relaxed);
        let unknown = self.unknown.load(Ordering::Relaxed);
        StreamDropSnapshot {
            client,
            proxy,
            upstream,
            unknown,
            total: client + proxy + upstream + unknown,
            since: unix_rfc3339(self.since_unix.load(Ordering::Relaxed)),
            last_proxy: unix_rfc3339(self.last_proxy_unix.load(Ordering::Relaxed)),
        }
    }
}

fn unix_rfc3339(value: i64) -> String {
    DateTime::from_timestamp(value, 0).map_or_else(String::new, |value| {
        value.to_rfc3339_opts(SecondsFormat::Secs, true)
    })
}

#[derive(Clone, Debug, Serialize)]
pub struct AccountStatus {
    pub id: String,
    pub provider: Provider,
    pub auth_mode: AuthMode,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub email: String,
    pub source: String,
    pub auth_checked: bool,
    pub auth_valid: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub refreshed: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct AccountUsageStatus {
    #[serde(flatten)]
    pub account: AccountStatus,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub active: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub plan_type: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub windows: Vec<UsageWindow>,
    #[serde(skip)]
    pub fresh: bool,
}

struct AccountRefInner {
    accounts: RwLock<Vec<Account>>,
    codex_store: CodexStore,
    claude_store: claude::Store,
    client: Client,
    usage_windows: std::sync::Mutex<HashMap<String, (Vec<UsageWindow>, SystemTime)>>,
}

#[derive(Clone)]
pub struct AccountRef(Arc<AccountRefInner>);

impl AccountRef {
    #[must_use]
    pub fn new(
        codex_store: CodexStore,
        claude_store: claude::Store,
        initial: Vec<Account>,
        client: Client,
    ) -> Self {
        Self(Arc::new(AccountRefInner {
            accounts: RwLock::new(initial),
            codex_store,
            claude_store,
            client,
            usage_windows: std::sync::Mutex::new(HashMap::new()),
        }))
    }

    #[must_use]
    pub fn all(&self) -> Vec<Account> {
        self.0
            .accounts
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    pub async fn reload(&self) -> anyhow::Result<Vec<Account>> {
        let mut loaded = self.0.codex_store.list()?;
        loaded.extend(self.0.claude_store.list_accounts().await?);
        *self
            .0
            .accounts
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = loaded.clone();
        Ok(loaded)
    }

    pub async fn refresh(&self, account: &Account) -> anyhow::Result<Account> {
        if account.auth_mode != AuthMode::Oauth {
            return Ok(account.clone());
        }
        let refreshed = match account.provider {
            Provider::Claude => {
                self.0
                    .claude_store
                    .refresh_account_if_expired(&self.0.client, account)
                    .await?
                    .0
            }
            Provider::Codex => {
                let Some(stored) = self.0.codex_store.find_stored(&account.id)? else {
                    return Ok(account.clone());
                };
                let (stored, _) = self
                    .0
                    .codex_store
                    .refresh_stored_if_expired(&self.0.client, stored, "proxy.account-refresh")
                    .await?;
                stored
                    .to_account(stored.source_path(&self.0.codex_store))
                    .unwrap_or_else(|| account.clone())
            }
            Provider::Kimi | Provider::Zai => account.clone(),
        };
        self.replace(refreshed.clone());
        Ok(refreshed)
    }

    fn replace(&self, account: Account) {
        let mut accounts = self
            .0
            .accounts
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(slot) = accounts.iter_mut().find(|current| {
            current.provider == account.provider && account_matches(current, &account.id)
        }) {
            *slot = account;
        } else {
            accounts.push(account);
        }
    }

    pub async fn statuses(&self, force: bool) -> Vec<AccountStatus> {
        let mut output = Vec::new();
        match self.0.codex_store.list_stored() {
            Ok(stored_accounts) => {
                for stored in stored_accounts {
                    let provider = stored.provider_or_default();
                    let mut status = AccountStatus {
                        id: stored.email.clone(),
                        provider,
                        auth_mode: if stored.is_api_key() {
                            AuthMode::ApiKey
                        } else {
                            AuthMode::Oauth
                        },
                        email: stored.email.clone(),
                        source: stored
                            .source_path(&self.0.codex_store)
                            .display()
                            .to_string(),
                        auth_checked: !stored.is_api_key(),
                        auth_valid: false,
                        refreshed: false,
                        error: String::new(),
                    };
                    if stored.is_api_key() {
                        output.push(status);
                        continue;
                    }
                    let result = if force {
                        self.0
                            .codex_store
                            .refresh_stored_force(&self.0.client, stored, "account-status.force")
                            .await
                    } else {
                        self.0
                            .codex_store
                            .refresh_stored_if_expired(
                                &self.0.client,
                                stored,
                                "account-status.if-expired",
                            )
                            .await
                    };
                    match result {
                        Ok((stored, refreshed)) => {
                            status.auth_valid = true;
                            status.refreshed = refreshed;
                            if let Some(account) =
                                stored.to_account(stored.source_path(&self.0.codex_store))
                            {
                                self.replace(account);
                            }
                        }
                        Err(error) => status.error = error.to_string(),
                    }
                    output.push(status);
                }
            }
            Err(error) => output.push(AccountStatus {
                id: String::new(),
                provider: Provider::Codex,
                auth_mode: AuthMode::Oauth,
                email: String::new(),
                source: String::new(),
                auth_checked: true,
                auth_valid: false,
                refreshed: false,
                error: error.to_string(),
            }),
        }
        for profile in self.0.claude_store.list_profiles() {
            let mut status = AccountStatus {
                id: profile.name.clone(),
                provider: Provider::Claude,
                auth_mode: AuthMode::Oauth,
                email: profile
                    .name
                    .strip_prefix("claude-")
                    .unwrap_or(&profile.name)
                    .to_owned(),
                source: self
                    .0
                    .claude_store
                    .claude_config_dir(&profile.name)
                    .display()
                    .to_string(),
                auth_checked: true,
                auth_valid: false,
                refreshed: false,
                error: String::new(),
            };
            match self
                .0
                .claude_store
                .refresh_credential_if_expired(&self.0.client, &profile)
                .await
            {
                Ok((account, refreshed)) => {
                    status.auth_valid = true;
                    status.refreshed = refreshed;
                    self.replace(account);
                }
                Err(error) => status.error = error.to_string(),
            }
            output.push(status);
        }
        output
    }

    async fn usage_windows(&self, account: &Account) -> anyhow::Result<(Vec<UsageWindow>, bool)> {
        let key = format!("{}\0{}", account.id, account.provider);
        let cached = self
            .0
            .usage_windows
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(&key)
            .cloned();
        if let Some((windows, at)) = &cached
            && at
                .elapsed()
                .is_ok_and(|elapsed| elapsed < Duration::from_secs(120))
        {
            return Ok((windows.clone(), true));
        }
        let result = match account.provider {
            Provider::Codex => accounts::fetch_codex_usage(&self.0.client, account).await,
            Provider::Claude => {
                if account.auth_mode == AuthMode::Oauth {
                    claude::fetch_fable_usage_windows(&self.0.client, &account.token).await
                } else {
                    Ok(Vec::new())
                }
            }
            Provider::Kimi | Provider::Zai => Ok(Vec::new()),
        };
        match result {
            Ok(windows) => {
                self.0
                    .usage_windows
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
                    .insert(key, (windows.clone(), SystemTime::now()));
                Ok((windows, true))
            }
            Err(_error)
                if cached.as_ref().is_some_and(|(_, at)| {
                    at.elapsed()
                        .is_ok_and(|elapsed| elapsed < Duration::from_secs(15 * 60))
                }) =>
            {
                Ok((cached.unwrap().0, false))
            }
            Err(error) => Err(error),
        }
    }

    pub async fn usage_statuses(&self) -> Vec<AccountUsageStatus> {
        let active_codex = self.0.codex_store.detect_active_account().ok().flatten();
        let active_claude = self.0.claude_store.active_profile();
        let mut output = Vec::new();
        for account in self.all() {
            let mut status = AccountUsageStatus {
                account: AccountStatus {
                    id: account.id.clone(),
                    provider: account.provider,
                    auth_mode: account.auth_mode,
                    email: account.email.clone(),
                    source: account.source.clone(),
                    auth_checked: account.auth_mode == AuthMode::Oauth,
                    auth_valid: account.auth_mode != AuthMode::Oauth,
                    refreshed: false,
                    error: String::new(),
                },
                active: match account.provider {
                    Provider::Codex => active_codex.as_deref() == Some(&account.id),
                    Provider::Claude => active_claude == account.id,
                    Provider::Kimi | Provider::Zai => false,
                },
                plan_type: if account.auth_mode == AuthMode::ApiKey {
                    format!("{} api key", account.provider)
                } else {
                    String::new()
                },
                windows: Vec::new(),
                fresh: false,
            };
            match self.refresh(&account).await {
                Ok(refreshed) => {
                    status.account.auth_valid = true;
                    match self.usage_windows(&refreshed).await {
                        Ok((windows, fresh)) => {
                            status.windows = windows;
                            status.fresh = fresh;
                        }
                        Err(error) => status.account.error = error.to_string(),
                    }
                }
                Err(error) => status.account.error = error.to_string(),
            }
            output.push(status);
        }
        output
    }
}

#[async_trait]
pub trait CredentialBroker: Send + Sync {
    async fn lease(&self, request: &broker::LeaseRequest) -> anyhow::Result<broker::Lease>;
    async fn report(&self, lease_id: &str, report: &broker::LeaseReport) -> anyhow::Result<()>;
    fn invalidate(&self, lease_id: &str);
}

#[async_trait]
impl CredentialBroker for BrokerClient {
    async fn lease(&self, request: &broker::LeaseRequest) -> anyhow::Result<broker::Lease> {
        BrokerClient::lease(self, request).await
    }

    async fn report(&self, lease_id: &str, report: &broker::LeaseReport) -> anyhow::Result<()> {
        BrokerClient::report(self, lease_id, report).await
    }

    fn invalidate(&self, lease_id: &str) {
        self.invalidate_lease(lease_id);
    }
}

#[derive(Clone)]
pub struct Server {
    pub upstreams: Upstreams,
    pub account_ref: Option<AccountRef>,
    pub static_accounts: Vec<Account>,
    pub sessions: Arc<session::Store>,
    pub scheduler: Arc<SchedulerRef>,
    pub credential_broker: Option<Arc<dyn CredentialBroker>>,
    pub active_sessions: Arc<ActiveSessions>,
    pub lifecycle: Arc<Lifecycle>,
    pub stream_drops: Arc<StreamDropStats>,
    pub session_leases: Arc<LeaseStore>,
    pub require_session_leases: bool,
    pub forward_session_headers: bool,
    pub admin_token: String,
    pub local_proxy_token: String,
    pub max_body_bytes: usize,
    pub usage_score_ttl: Duration,
    pub transcripts: Option<Arc<Recorder>>,
    pub cache_flight: Arc<SingleFlight>,
    pub claude_fable_api_key: String,
    pub fable_bedrock_primary: bool,
    pub bedrock: Option<Arc<BedrockConfig>>,
    client: Client,
    upload_limiter: Arc<Semaphore>,
}

impl Server {
    pub fn new(
        sessions: Arc<session::Store>,
        scheduler: Arc<SchedulerRef>,
    ) -> anyhow::Result<Self> {
        let client = outbound_client()?;
        Ok(Self {
            upstreams: Upstreams::default(),
            account_ref: None,
            static_accounts: Vec::new(),
            sessions,
            scheduler,
            credential_broker: None,
            active_sessions: Arc::new(ActiveSessions::default()),
            lifecycle: Arc::new(Lifecycle::default()),
            stream_drops: Arc::new(StreamDropStats::default()),
            session_leases: Arc::new(LeaseStore::default()),
            require_session_leases: false,
            forward_session_headers: false,
            admin_token: String::new(),
            local_proxy_token: String::new(),
            max_body_bytes: 4 << 20,
            usage_score_ttl: Duration::from_secs(30),
            transcripts: None,
            cache_flight: Arc::new(SingleFlight::default()),
            claude_fable_api_key: String::new(),
            fable_bedrock_primary: false,
            bedrock: None,
            client,
            upload_limiter: Arc::new(Semaphore::new(4)),
        })
    }

    pub fn router(self: Arc<Self>) -> Router {
        Router::new().fallback(any(dispatch)).with_state(self)
    }

    #[must_use]
    pub fn accounts(&self) -> Vec<Account> {
        let mut output = self.static_accounts.clone();
        if let Some(reference) = &self.account_ref {
            output.extend(reference.all());
        }
        output
    }

    pub(crate) fn authorize_admin(&self, remote: Option<SocketAddr>, headers: &HeaderMap) -> bool {
        if remote.is_none_or(|remote| remote.ip().is_loopback())
            || self.admin_token.trim().is_empty()
        {
            return true;
        }
        let presented = headers
            .get("x-subrouter-admin-token")
            .and_then(|value| value.to_str().ok())
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .or_else(|| bearer_token(headers));
        constant_time_equal(presented.unwrap_or_default(), self.admin_token.trim())
    }

    fn authorize_session_lease_admin(
        &self,
        remote: Option<SocketAddr>,
        headers: &HeaderMap,
    ) -> bool {
        remote.is_none_or(|remote| remote.ip().is_loopback())
            || (!self.admin_token.trim().is_empty() && self.authorize_admin(remote, headers))
    }

    fn local_proxy_authorized(&self, headers: &HeaderMap) -> bool {
        self.local_proxy_token.trim().is_empty()
            || constant_time_equal(
                bearer_token(headers).unwrap_or_default(),
                self.local_proxy_token.trim(),
            )
    }

    fn allow_draining(&self, agent: &str, session_id: &str) -> bool {
        self.active_sessions.active(agent, session_id)
            || self.sessions.get(agent, session_id).is_some()
    }

    #[allow(clippy::too_many_arguments)]
    fn account_for_request(
        &self,
        provider: Provider,
        agent: &str,
        session_id: &str,
        user_email: &str,
        forced_account: Option<&str>,
        model: &str,
        path: &str,
        oauth_only: bool,
    ) -> anyhow::Result<Account> {
        let mut accounts = filter_accounts_for_provider(self.accounts(), provider);
        if oauth_only || (provider == Provider::Codex && chatgpt_backend_path(path)) {
            accounts.retain(|account| account.auth_mode == AuthMode::Oauth);
        }
        if provider == Provider::Claude
            && claude_fable_model(model)
            && self.claude_fallback_enabled()
        {
            accounts.retain(|account| account.auth_mode == AuthMode::Oauth);
        }
        if let Some(forced) = forced_account.filter(|value| !value.trim().is_empty()) {
            let account = accounts
                .iter()
                .find(|account| account_matches(account, forced))
                .cloned()
                .ok_or_else(|| anyhow!("requested account {forced:?} not found"))?;
            if chatgpt_backend_path(path) && account.auth_mode != AuthMode::Oauth {
                bail!("requested account {forced:?} cannot be used for ChatGPT backend paths");
            }
            self.sessions
                .put(agent, session_id, &account.id, user_email)?;
            return Ok(account);
        }
        let pool_model = if provider == Provider::Claude {
            claude_pool_model(model)
        } else {
            model.to_owned()
        };
        let scheduler = self
            .scheduler
            .get()
            .for_model(&pool_model)
            .with_live_debits(self.scheduler.live_debits())
            .with_session_counts(self.sessions.count_by_account());
        if let Some(assignment) = self.sessions.get(agent, session_id)
            && let Some(account) = accounts
                .iter()
                .find(|account| account_matches(account, &assignment.account_id))
                .cloned()
        {
            let active = self.active_sessions.active(agent, session_id);
            let reusable = !scheduler.exhausted(account.provider, &account.id)
                && (active
                    || account.provider != Provider::Codex
                    || account.auth_mode != AuthMode::Oauth
                    || scheduler.usable_for_new_session(account.provider, &account.id));
            if reusable {
                if !user_email.is_empty() && assignment.user_email != user_email {
                    self.sessions
                        .put(agent, session_id, &account.id, user_email)?;
                }
                return Ok(account);
            }
        }
        let account = scheduler.pick(&accounts)?;
        if provider == Provider::Claude
            && account.auth_mode == AuthMode::Oauth
            && scheduler.exhausted(account.provider, &account.id)
        {
            bail!("no non-exhausted {provider} accounts available");
        }
        self.sessions
            .put(agent, session_id, &account.id, user_email)?;
        Ok(account)
    }

    async fn refresh_account(&self, account: &Account) -> anyhow::Result<Account> {
        match &self.account_ref {
            Some(reference) => reference.refresh(account).await,
            None => Ok(account.clone()),
        }
    }

    #[allow(clippy::too_many_arguments)]
    async fn alternate_account(
        &self,
        provider: Provider,
        agent: &str,
        session_id: &str,
        user: &str,
        pool_model: &str,
        tried: &HashSet<String>,
        oauth_only: bool,
    ) -> anyhow::Result<Account> {
        let mut candidates = filter_accounts_for_provider(self.accounts(), provider);
        candidates.retain(|account| {
            !tried.contains(&account.id) && (!oauth_only || account.auth_mode == AuthMode::Oauth)
        });
        let scheduler = self
            .scheduler
            .get()
            .for_model(pool_model)
            .with_live_debits(self.scheduler.live_debits())
            .with_session_counts(self.sessions.count_by_account());
        candidates.retain(|account| {
            account.auth_mode == AuthMode::ApiKey
                || !scheduler.exhausted(account.provider, &account.id)
        });
        let account = scheduler.pick(&candidates)?;
        let account = self.refresh_account(&account).await?;
        self.sessions.put(agent, session_id, &account.id, user)?;
        Ok(account)
    }

    fn claude_fallback_enabled(&self) -> bool {
        !self.claude_fable_api_key.trim().is_empty()
            || self
                .bedrock
                .as_ref()
                .is_some_and(|config| config.configured())
    }

    /// Dispatches one request through this server. Wrappers such as the
    /// multi-tenant router use this entry point after selecting isolated state.
    pub async fn handle(self: Arc<Self>, request: Request<Body>) -> Response {
        dispatch_request(self, request).await
    }
}

#[axum::debug_handler]
async fn dispatch(State(server): State<Arc<Server>>, request: Request<Body>) -> Response {
    dispatch_request(server, request).await
}

async fn dispatch_request(server: Arc<Server>, request: Request<Body>) -> Response {
    let remote = request
        .extensions()
        .get::<ConnectInfo<ClientAddr>>()
        .and_then(|value| value.0.0);
    let (mut parts, body) = request.into_parts();
    let websocket = WebSocketUpgrade::from_request_parts(&mut parts, &server)
        .await
        .ok();
    let request = Request::from_parts(parts, body);
    let path = request.uri().path().to_owned();
    if path == "/_subrouter/health" {
        return json_response(StatusCode::OK, &json!({"ok":true}));
    }
    if path == "/_subrouter/ready" {
        let ready = !server.lifecycle.draining();
        return json_response(
            if ready {
                StatusCode::OK
            } else {
                StatusCode::SERVICE_UNAVAILABLE
            },
            &json!({"ok":ready,"draining":!ready}),
        );
    }
    if path == "/_subrouter/stream-stats" {
        return json_response(StatusCode::OK, &server.stream_drops.snapshot());
    }
    if path.starts_with("/bedrock/") {
        return match &server.bedrock {
            Some(config) => config.handle_gateway(request).await,
            None => text_response(
                StatusCode::SERVICE_UNAVAILABLE,
                "bedrock gateway not configured\n",
            ),
        };
    }
    if path == "/internal/v1/session-leases" || path.starts_with("/internal/v1/session-leases/") {
        if !server.authorize_session_lease_admin(remote, request.headers()) {
            return text_response(StatusCode::UNAUTHORIZED, "admin token required\n");
        }
        return handle_session_leases(server, remote, request).await;
    }
    if path.starts_with("/_subrouter/") {
        if !server.authorize_admin(remote, request.headers()) {
            return text_response(StatusCode::UNAUTHORIZED, "admin token required\n");
        }
        return handle_control(server, remote, request).await;
    }
    proxy_request(server, remote, websocket, request).await
}

async fn handle_control(
    server: Arc<Server>,
    remote: Option<SocketAddr>,
    request: Request<Body>,
) -> Response {
    let method = request.method().clone();
    let path = request.uri().path();
    match (method.clone(), path) {
        (Method::POST, "/_subrouter/drain") => {
            if remote.is_some_and(|remote| !remote.ip().is_loopback()) {
                return text_response(
                    StatusCode::FORBIDDEN,
                    "drain is only available from loopback\n",
                );
            }
            server.lifecycle.drain();
            json_response(StatusCode::OK, &server.lifecycle.status(true))
        }
        (Method::GET, "/_subrouter/drain-status") => {
            json_response(StatusCode::OK, &server.lifecycle.status(false))
        }
        (Method::GET, "/_subrouter/accounts") => {
            #[derive(Serialize)]
            struct SafeAccount<'a> {
                id: &'a str,
                provider: Provider,
                auth_mode: AuthMode,
                #[serde(skip_serializing_if = "str::is_empty")]
                email: &'a str,
                source: &'a str,
            }
            let accounts = server.accounts();
            let safe: Vec<_> = accounts
                .iter()
                .map(|account| SafeAccount {
                    id: &account.id,
                    provider: account.provider,
                    auth_mode: account.auth_mode,
                    email: &account.email,
                    source: &account.source,
                })
                .collect();
            json_response(StatusCode::OK, &safe)
        }
        (Method::GET | Method::POST, "/_subrouter/account-status") => {
            let statuses = if let Some(reference) = &server.account_ref {
                reference.statuses(method == Method::POST).await
            } else {
                server
                    .accounts()
                    .into_iter()
                    .map(|account| AccountStatus {
                        id: account.id,
                        provider: account.provider,
                        auth_mode: account.auth_mode,
                        email: account.email,
                        source: account.source,
                        auth_checked: false,
                        auth_valid: false,
                        refreshed: false,
                        error: String::new(),
                    })
                    .collect()
            };
            json_response(StatusCode::OK, &statuses)
        }
        (Method::GET, "/_subrouter/usage-status") => {
            let statuses = if let Some(reference) = &server.account_ref {
                reference.usage_statuses().await
            } else {
                Vec::new()
            };
            update_scheduler_from_usage(&server, &statuses);
            json_response(StatusCode::OK, &statuses)
        }
        (Method::GET, "/_subrouter/reset-credits") => reset_credits_response(&server).await,
        (Method::POST, "/_subrouter/rate-limit-reset") => {
            rate_limit_reset_response(&server, request.uri().query()).await
        }
        (Method::GET, "/_subrouter/bedrock-cost") => {
            let summary = server
                .bedrock
                .as_ref()
                .map_or_else(bedrock::CostSummary::default, |config| {
                    config.cost_summary()
                });
            json_response(StatusCode::OK, &summary)
        }
        (Method::POST, "/_subrouter/reload-accounts") => {
            if remote.is_some_and(|remote| !remote.ip().is_loopback()) {
                return text_response(
                    StatusCode::FORBIDDEN,
                    "reload-accounts is only available from loopback\n",
                );
            }
            let Some(reference) = &server.account_ref else {
                return text_response(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "account reload is not configured\n",
                );
            };
            match reference.reload().await {
                Ok(accounts) => json_response(
                    StatusCode::OK,
                    &json!({"ok":true,"accounts":accounts.len(),"usage_refreshed":0}),
                ),
                Err(error) => {
                    text_response(StatusCode::INTERNAL_SERVER_ERROR, &format!("{error}\n"))
                }
            }
        }
        (Method::GET, "/_subrouter/sessions") => {
            json_response(StatusCode::OK, &server.sessions.all())
        }
        (Method::GET, "/_subrouter/transcripts") => match server.transcripts.as_ref() {
            Some(recorder) => match transcript::list_summaries(recorder.dir()) {
                Ok(summaries) => json_response(StatusCode::OK, &summaries),
                Err(error) => {
                    text_response(StatusCode::INTERNAL_SERVER_ERROR, &format!("{error}\n"))
                }
            },
            None => json_response(StatusCode::OK, &Vec::<Value>::new()),
        },
        (Method::GET, "/_subrouter/dashboard") => dashboard_response(&server),
        (Method::GET, path) if path.starts_with("/_subrouter/transcripts/") => {
            transcript_detail(&server, path)
        }
        (
            _,
            "/_subrouter/drain"
            | "/_subrouter/drain-status"
            | "/_subrouter/accounts"
            | "/_subrouter/account-status"
            | "/_subrouter/usage-status"
            | "/_subrouter/reset-credits"
            | "/_subrouter/rate-limit-reset"
            | "/_subrouter/bedrock-cost"
            | "/_subrouter/reload-accounts"
            | "/_subrouter/sessions"
            | "/_subrouter/transcripts"
            | "/_subrouter/dashboard",
        ) => text_response(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n"),
        _ => text_response(StatusCode::NOT_FOUND, "404 page not found\n"),
    }
}

#[derive(Serialize)]
struct ResetCreditsAccount {
    email: String,
    count: usize,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    credits: Vec<accounts::RateLimitResetCredit>,
    #[serde(skip_serializing_if = "String::is_empty")]
    error: String,
}

async fn reset_credits_response(server: &Server) -> Response {
    let mut entries = Vec::new();
    for account in server.accounts().into_iter().filter(|account| {
        account.provider == Provider::Codex && account.auth_mode == AuthMode::Oauth
    }) {
        let account = match server.refresh_account(&account).await {
            Ok(account) => account,
            Err(error) => {
                entries.push(ResetCreditsAccount {
                    email: account.email,
                    count: 0,
                    credits: Vec::new(),
                    error: error.to_string(),
                });
                continue;
            }
        };
        match accounts::list_rate_limit_reset_credits(&server.client, &account).await {
            Ok(credits) => {
                let credits = credits
                    .into_iter()
                    .filter(|credit| credit.status.is_empty() || credit.status == "available")
                    .collect::<Vec<_>>();
                entries.push(ResetCreditsAccount {
                    email: account.email,
                    count: credits.len(),
                    credits,
                    error: String::new(),
                });
            }
            Err(error) => entries.push(ResetCreditsAccount {
                email: account.email,
                count: 0,
                credits: Vec::new(),
                error: error.to_string(),
            }),
        }
    }
    entries.sort_by(|left, right| left.email.cmp(&right.email));
    json_response(StatusCode::OK, &json!({"accounts":entries}))
}

#[derive(Serialize)]
struct ResetResult {
    email: String,
    eligible: bool,
    reset: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    dry_run: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    credit: Option<accounts::RateLimitResetCredit>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    windows_before: Vec<UsageWindow>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    windows_after: Vec<UsageWindow>,
    #[serde(skip_serializing_if = "String::is_empty")]
    error: String,
}

async fn rate_limit_reset_response(server: &Server, query: Option<&str>) -> Response {
    let parameters = url::form_urlencoded::parse(query.unwrap_or_default().as_bytes())
        .collect::<HashMap<_, _>>();
    let email = parameters
        .get("email")
        .map(|value| value.trim().to_owned())
        .unwrap_or_default();
    let all = parameters
        .get("all")
        .is_some_and(|value| boolean_query(value));
    let dry_run = parameters
        .get("dry_run")
        .is_some_and(|value| boolean_query(value));
    if !email.is_empty() && all {
        return text_response(
            StatusCode::BAD_REQUEST,
            "pass either email or all, not both\n",
        );
    }
    let mut candidates = Vec::new();
    for account in server.accounts().into_iter().filter(|account| {
        account.provider == Provider::Codex && account.auth_mode == AuthMode::Oauth
    }) {
        if !email.is_empty() && !account_matches(&account, &email) {
            continue;
        }
        let account = match server.refresh_account(&account).await {
            Ok(account) => account,
            Err(error) => {
                candidates.push((account, Vec::new(), Some(error.to_string()), 0));
                continue;
            }
        };
        match accounts::fetch_codex_usage_details(&server.client, &account).await {
            Ok(details) => {
                let cooked = details
                    .windows
                    .iter()
                    .any(|window| window.used_percent >= 100.0);
                let available = details
                    .complimentary_reset
                    .as_ref()
                    .is_some_and(|reset| reset.available);
                if cooked && available {
                    let downtime = details
                        .windows
                        .iter()
                        .filter(|window| window.used_percent >= 100.0)
                        .map(|window| window.reset_after_seconds)
                        .max()
                        .unwrap_or_default();
                    candidates.push((account, details.windows, None, downtime));
                } else if !email.is_empty() {
                    candidates.push((
                        account,
                        details.windows,
                        Some(format!("account is not eligible for a reset (cooked={cooked}, credit={available})")),
                        0,
                    ));
                }
            }
            Err(error) if !email.is_empty() => {
                candidates.push((account, Vec::new(), Some(error.to_string()), 0))
            }
            Err(_) => {}
        }
    }
    if !all && email.is_empty() && candidates.len() > 1 {
        candidates.sort_by(|left, right| {
            right
                .3
                .cmp(&left.3)
                .then_with(|| left.0.email.cmp(&right.0.email))
        });
        candidates.truncate(1);
    }
    let mut results = Vec::new();
    let mut reset = 0;
    for (account, before, error, _) in candidates {
        if let Some(error) = error {
            results.push(ResetResult {
                email: account.email,
                eligible: false,
                reset: false,
                dry_run,
                credit: None,
                windows_before: before,
                windows_after: Vec::new(),
                error,
            });
            continue;
        }
        if dry_run {
            results.push(ResetResult {
                email: account.email,
                eligible: true,
                reset: false,
                dry_run: true,
                credit: None,
                windows_before: before,
                windows_after: Vec::new(),
                error: String::new(),
            });
            continue;
        }
        match accounts::redeem_rate_limit_reset(&server.client, &account).await {
            Ok(credit) => {
                reset += 1;
                let after = accounts::fetch_codex_usage_details(&server.client, &account)
                    .await
                    .map(|details| details.windows)
                    .unwrap_or_default();
                results.push(ResetResult {
                    email: account.email,
                    eligible: true,
                    reset: true,
                    dry_run: false,
                    credit: Some(credit),
                    windows_before: before,
                    windows_after: after,
                    error: String::new(),
                });
            }
            Err(error) => results.push(ResetResult {
                email: account.email,
                eligible: true,
                reset: false,
                dry_run: false,
                credit: None,
                windows_before: before,
                windows_after: Vec::new(),
                error: error.to_string(),
            }),
        }
    }
    if !email.is_empty() && results.is_empty() {
        return text_response(StatusCode::NOT_FOUND, "account not found\n");
    }
    json_response(
        StatusCode::OK,
        &json!({"dry_run":dry_run,"reset":reset,"results":results}),
    )
}

fn boolean_query(value: &str) -> bool {
    matches!(
        value.trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

fn dashboard_response(server: &Server) -> Response {
    let (analytics, summaries) = match &server.transcripts {
        Some(recorder) => (
            transcript::analyze(recorder.dir()).unwrap_or_default(),
            transcript::list_summaries(recorder.dir()).unwrap_or_default(),
        ),
        None => (transcript::Analytics::default(), Vec::new()),
    };
    let analytics = serde_json::to_string(&analytics).unwrap_or_else(|_| "{}".into());
    let rows = summaries
        .iter()
        .map(|summary| {
            format!(
                "<tr><td>{}</td><td><a href=\"/_subrouter/transcripts/{}/{}\">{}</a></td><td>{}</td><td>{}</td></tr>",
                html_escape(&summary.agent_type),
                urlencoding_component(&summary.agent_type),
                urlencoding_component(&summary.session_id),
                html_escape(&summary.session_id),
                html_escape(&summary.account),
                summary.usage.total_tokens
            )
        })
        .collect::<String>();
    Html(format!(
        "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>Subrouter</title><style>body{{font:14px system-ui;margin:2rem;max-width:1100px}}table{{border-collapse:collapse;width:100%}}th,td{{padding:.5rem;border-bottom:1px solid #ddd;text-align:left}}code{{white-space:pre-wrap}}</style></head><body><h1>Subrouter</h1><p>Requests: {} · Tokens: {}</p><table><thead><tr><th>Agent</th><th>Session</th><th>Account</th><th>Tokens</th></tr></thead><tbody>{rows}</tbody></table><script type=\"application/json\" id=\"analytics\">{analytics}</script></body></html>",
        serde_json::from_str::<Value>(&analytics).ok().and_then(|value| value.pointer("/totals/requests").and_then(Value::as_i64)).unwrap_or(0),
        serde_json::from_str::<Value>(&analytics).ok().and_then(|value| value.pointer("/totals/total_tokens").and_then(Value::as_i64)).unwrap_or(0),
    ))
    .into_response()
}

fn transcript_detail(server: &Server, path: &str) -> Response {
    let Some(recorder) = &server.transcripts else {
        return text_response(StatusCode::NOT_FOUND, "transcripts are disabled\n");
    };
    let relative = path
        .trim_start_matches("/_subrouter/transcripts/")
        .trim_matches('/');
    let mut parts = relative.split('/');
    let (Some(agent), Some(session_id)) = (parts.next(), parts.next()) else {
        return text_response(StatusCode::NOT_FOUND, "404 page not found\n");
    };
    let raw = parts.next() == Some("raw");
    if parts.next().is_some() {
        return text_response(StatusCode::NOT_FOUND, "404 page not found\n");
    }
    let result = if raw {
        transcript::read_raw_session(recorder.dir(), agent, session_id)
            .map(|value| serde_json::to_value(value).unwrap_or_default())
    } else {
        transcript::read_sanitized_session(recorder.dir(), agent, session_id)
            .map(|value| serde_json::to_value(value).unwrap_or_default())
    };
    match result {
        Ok(value) => json_response(StatusCode::OK, &value),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            text_response(StatusCode::NOT_FOUND, "transcript not found\n")
        }
        Err(error) => text_response(StatusCode::INTERNAL_SERVER_ERROR, &format!("{error}\n")),
    }
}

async fn handle_session_leases(
    server: Arc<Server>,
    remote: Option<SocketAddr>,
    request: Request<Body>,
) -> Response {
    let method = request.method().clone();
    let path = request.uri().path().to_owned();
    if path == "/internal/v1/session-leases" {
        if method != Method::POST {
            return text_response(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n");
        }
        if server.lifecycle.draining() {
            return text_response(StatusCode::SERVICE_UNAVAILABLE, "subrouter is draining\n");
        }
        let host = request
            .headers()
            .get(http::header::HOST)
            .and_then(|value| value.to_str().ok())
            .unwrap_or("127.0.0.1:31415")
            .to_owned();
        let body = match to_bytes(request.into_body(), 64 << 10).await {
            Ok(body) => body,
            Err(_) => {
                return text_response(StatusCode::BAD_REQUEST, "invalid session lease request\n");
            }
        };
        let mut lease_request: SessionLeaseRequest = match serde_json::from_slice(&body) {
            Ok(request) => request,
            Err(_) => {
                return text_response(StatusCode::BAD_REQUEST, "invalid session lease request\n");
            }
        };
        let (provider, model) = match lease_request.normalize_and_validate() {
            Ok(value) => value,
            Err(error) => return text_response(StatusCode::BAD_REQUEST, &format!("{error}\n")),
        };
        let proxy_base_url = if lease_request.proxy_base_url.is_empty() {
            format!("http://{host}")
        } else {
            lease_request.proxy_base_url.clone()
        };
        if !valid_http_base_url(&proxy_base_url) {
            return text_response(
                StatusCode::BAD_REQUEST,
                "proxyBaseUrl must be an http or https base URL\n",
            );
        }
        let routing_key = lease_request.routing_key(provider, &model);
        let account = match server.account_for_request(
            provider,
            &lease_request.agent,
            &routing_key,
            "",
            None,
            &model,
            lease_endpoint(provider),
            provider == Provider::Codex,
        ) {
            Ok(account) => match server.refresh_account(&account).await {
                Ok(account) => account,
                Err(_) => {
                    return text_response(
                        StatusCode::SERVICE_UNAVAILABLE,
                        "no account is available for the requested lease\n",
                    );
                }
            },
            Err(_) => {
                return text_response(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "no account is available for the requested lease\n",
                );
            }
        };
        let template = leases::LeaseTemplate {
            scope_key: lease_request.scope_key(provider, &model),
            organization_id: lease_request.organization_id,
            workspace_id: lease_request.workspace_id,
            conversation_id: lease_request.conversation_id,
            invocation_id: lease_request.invocation_id,
            session_key: routing_key,
            agent: lease_request.agent,
            provider,
            account_id: account.id,
            auth_mode: account.auth_mode,
            model,
            proxy_base_url,
        };
        return match server.session_leases.put(template) {
            Ok(lease) => {
                let mut response = json_response(StatusCode::OK, &lease.response());
                response.headers_mut().insert(
                    http::header::CACHE_CONTROL,
                    HeaderValue::from_static("no-store"),
                );
                response
            }
            Err(_) => text_response(StatusCode::INTERNAL_SERVER_ERROR, "create session lease\n"),
        };
    }
    let relative = path
        .trim_start_matches("/internal/v1/session-leases/")
        .trim_matches('/');
    let parts: Vec<_> = relative.split('/').collect();
    if relative.is_empty() || parts.len() > 2 {
        return text_response(StatusCode::NOT_FOUND, "404 page not found\n");
    }
    if parts.len() == 2 {
        if parts[1] != "renew" {
            return text_response(StatusCode::NOT_FOUND, "404 page not found\n");
        }
        if method != Method::POST {
            return text_response(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n");
        }
        let token = request
            .headers()
            .get("x-subrouter-lease")
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .trim();
        if !leases::looks_like_token(token) {
            return text_response(StatusCode::UNAUTHORIZED, "session lease token required\n");
        }
        return match server.session_leases.renew(parts[0], token) {
            Ok(lease) => {
                let mut response = json_response(StatusCode::OK, &lease.response());
                response.headers_mut().insert(
                    http::header::CACHE_CONTROL,
                    HeaderValue::from_static("no-store"),
                );
                response
            }
            Err(LeaseError::NotFound) => {
                text_response(StatusCode::NOT_FOUND, "404 page not found\n")
            }
            Err(LeaseError::Invalid) => text_response(
                StatusCode::UNAUTHORIZED,
                "invalid or expired session lease\n",
            ),
            Err(LeaseError::Generation) => {
                text_response(StatusCode::INTERNAL_SERVER_ERROR, "renew session lease\n")
            }
        };
    }
    if method != Method::DELETE {
        return text_response(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n");
    }
    server.session_leases.release(parts[0]);
    let _ = (remote, headers_for_lint(&request));
    StatusCode::NO_CONTENT.into_response()
}

fn headers_for_lint(request: &Request<Body>) -> usize {
    request.headers().len()
}

async fn proxy_request(
    server: Arc<Server>,
    remote: Option<SocketAddr>,
    websocket: Option<WebSocketUpgrade>,
    request: Request<Body>,
) -> Response {
    if request.method() == Method::HEAD && matches!(request.uri().path(), "" | "/") {
        return StatusCode::NO_CONTENT.into_response();
    }
    let (parts, body) = request.into_parts();
    let header_probe = request_for_session(&parts, Bytes::new());
    let initial_agent = session::extract_agent_type(&header_probe);
    let initial_provider = provider_for_request(&initial_agent, parts.uri.path());
    let needs_replay_buffer = retryable_request(initial_provider, &parts.method, parts.uri.path())
        || leases::presented_token(&parts.headers).is_some();
    let buffer_limit = if needs_replay_buffer {
        MAX_PROXY_BODY_BYTES
    } else {
        server.max_body_bytes.max(8 << 20)
    };
    let request_body = match prepare_request_body(body, buffer_limit).await {
        Ok(body) => body,
        Err(error) => {
            return text_response(
                StatusCode::BAD_GATEWAY,
                &format!("read request body: {error}\n"),
            );
        }
    };
    let wire_body = request_body.probe();
    let probe = request_for_session(&parts, wire_body.clone());
    let mut agent = session::extract_agent_type(&probe);
    let mut session_id = session::extract_id(
        &probe,
        server.max_body_bytes,
        &remote.map_or_else(String::new, |value| value.to_string()),
    );
    let mut provider = provider_for_request(&agent, parts.uri.path());
    let mut bound_lease = None;
    if let Some(token) = leases::presented_token(&parts.headers) {
        let lease = match server.session_leases.resolve(&token) {
            Ok(lease) => lease,
            Err(error) => return text_response(StatusCode::UNAUTHORIZED, &format!("{error}\n")),
        };
        if !lease.allows_endpoint(&parts.method, parts.uri.path()) {
            return text_response(
                StatusCode::FORBIDDEN,
                "session lease does not allow the requested endpoint\n",
            );
        }
        let validation = if !lease.model.is_empty() && !request_body.is_buffered() {
            Err(anyhow!("request body is too large to validate"))
        } else {
            lease.validate_model(&parts.uri, &parts.headers, &wire_body)
        };
        if let Err(error) = validation {
            warn!(provider = %lease.provider, model = %lease.model, path = %parts.uri.path(), reason = %error, "session lease model request rejected");
            return text_response(
                StatusCode::FORBIDDEN,
                "session lease does not allow the requested model\n",
            );
        }
        agent.clone_from(&lease.agent);
        session_id.clone_from(&lease.session_key);
        provider = lease.provider;
        bound_lease = Some(lease);
    } else if server.require_session_leases {
        return text_response(StatusCode::UNAUTHORIZED, "session lease required\n");
    }
    if !server.local_proxy_authorized(&parts.headers) {
        return text_response(StatusCode::UNAUTHORIZED, "unauthorized\n");
    }
    if server.lifecycle.draining() && !server.allow_draining(&agent, &session_id) {
        return text_response(StatusCode::SERVICE_UNAVAILABLE, "subrouter is draining\n");
    }
    let lifecycle_guard = server.lifecycle.begin();
    refresh_scheduler_if_stale(Arc::clone(&server));
    let user = session::extract_user_email(&probe).unwrap_or_default();
    let model = session::extract_model(&probe, server.max_body_bytes).unwrap_or_default();
    if server.fable_bedrock_primary
        && provider == Provider::Claude
        && parts.method == Method::POST
        && parts.uri.path().ends_with("/v1/messages")
        && claude_fable_model(&model)
        && let Some(config) = server.bedrock.as_ref().filter(|config| config.configured())
    {
        if let Some(buffered_body) = request_body.buffered() {
            match config.fable_response(buffered_body).await {
                Ok(response) if response.status.is_success() => {
                    drop(lifecycle_guard);
                    return response.into_axum();
                }
                Ok(response) => warn!(
                    status = response.status.as_u16(),
                    "Fable Bedrock-primary response unusable, falling through to subscription pool"
                ),
                Err(error) => {
                    warn!(%error, "Fable Bedrock-primary request failed, falling through to subscription pool")
                }
            }
        } else {
            warn!(
                "Fable Bedrock-primary request exceeds replay buffer, falling through to subscription pool"
            );
        }
    }
    let session_agent = agent_for_provider_session(&agent, provider);
    let extracted_forced = session::extract_account_id(&probe);
    let forced = bound_lease
        .as_ref()
        .map(|lease| lease.account_id.as_str())
        .or(extracted_forced.as_deref());

    let (account, broker_lease) = if let Some(broker) = &server.credential_broker {
        let request = broker::LeaseRequest {
            provider,
            required_auth_mode: (provider == Provider::Codex
                && chatgpt_backend_path(parts.uri.path()))
            .then_some(AuthMode::Oauth),
            agent_type: session_agent.clone(),
            session_id: session_id.clone(),
            user_email: user.clone(),
            prefer_account_id: forced.unwrap_or_default().into(),
            model: model.clone(),
        };
        match broker.lease(&request).await {
            Ok(lease) => (lease.account.clone(), Some(lease)),
            Err(error) => {
                return text_response(StatusCode::SERVICE_UNAVAILABLE, &format!("{error}\n"));
            }
        }
    } else {
        let account = match server.account_for_request(
            provider,
            &session_agent,
            &session_id,
            &user,
            forced,
            &model,
            parts.uri.path(),
            bound_lease
                .as_ref()
                .is_some_and(|lease| lease.provider == Provider::Codex),
        ) {
            Ok(account) => account,
            Err(error) => {
                if let Some(body) = request_body.buffered()
                    && let Some(response) =
                        try_fable_fallback(&server, provider, &model, &parts, body.clone()).await
                {
                    return response;
                }
                return text_response(StatusCode::SERVICE_UNAVAILABLE, &format!("{error}\n"));
            }
        };
        match server.refresh_account(&account).await {
            Ok(account) => (account, None),
            Err(error) => {
                return text_response(
                    StatusCode::SERVICE_UNAVAILABLE,
                    &format!("refresh selected account: {error}\n"),
                );
            }
        }
    };
    if let Some(lease) = &bound_lease
        && !lease.allows_account(&account)
    {
        return text_response(
            StatusCode::SERVICE_UNAVAILABLE,
            "session lease account binding is unavailable\n",
        );
    }
    if account.token.is_empty() {
        return text_response(
            StatusCode::SERVICE_UNAVAILABLE,
            "selected account has no usable credential\n",
        );
    }
    let active_guard = active_session_request(
        &session_agent,
        &parts.method,
        parts.uri.path(),
        websocket.is_some(),
    )
    .then(|| server.active_sessions.begin(&session_agent, &session_id))
    .flatten();
    let prepared = PreparedRequest {
        method: parts.method,
        uri: parts.uri,
        headers: parts.headers,
        body: request_body,
        provider,
        agent: session_agent,
        session_id,
        user,
        model,
        account,
        broker_lease,
        bound_lease,
        disable_failover: false,
        remote,
    };
    if let Some(websocket) = websocket {
        return proxy_websocket(server, websocket, prepared, lifecycle_guard, active_guard).await;
    }
    if prepared.method == Method::GET && coalescable_path(prepared.uri.path()) {
        let key = flight_key(
            &prepared.method,
            &prepared.uri,
            &authenticated_headers(&prepared.headers, &prepared.account),
        );
        let operation_server = Arc::clone(&server);
        let operation_prepared = prepared.clone();
        let response = server
            .cache_flight
            .run(key, move || async move {
                execute_buffered(operation_server, operation_prepared).await
            })
            .await;
        drop((lifecycle_guard, active_guard));
        return buffered_into_response(&response);
    }
    match execute_upstream(Arc::clone(&server), prepared).await {
        Ok(result) => proxy_result_into_response(server, result, lifecycle_guard, active_guard),
        Err(error) => {
            error!(error = %error, "proxy request failed");
            text_response(StatusCode::BAD_GATEWAY, "upstream request failed\n")
        }
    }
}

type IncomingBodyStream = Pin<Box<dyn Stream<Item = Result<Bytes, axum::Error>> + Send>>;

#[derive(Clone)]
enum RequestBody {
    Buffered(Bytes),
    Streaming {
        stream: Arc<Mutex<Option<IncomingBodyStream>>>,
        probe: Bytes,
    },
}

impl RequestBody {
    fn is_buffered(&self) -> bool {
        matches!(self, Self::Buffered(_))
    }

    fn buffered(&self) -> Option<&Bytes> {
        match self {
            Self::Buffered(body) => Some(body),
            Self::Streaming { .. } => None,
        }
    }

    fn probe(&self) -> Bytes {
        match self {
            Self::Buffered(body) => body.clone(),
            Self::Streaming { probe, .. } => probe.clone(),
        }
    }

    fn take_stream(&self) -> anyhow::Result<IncomingBodyStream> {
        let Self::Streaming { stream, .. } = self else {
            bail!("buffered request body does not have a one-shot stream");
        };
        stream
            .lock()
            .map_err(|_| anyhow!("request body stream lock poisoned"))?
            .take()
            .ok_or_else(|| anyhow!("streaming request body was already consumed"))
    }
}

async fn prepare_request_body(body: Body, buffer_limit: usize) -> anyhow::Result<RequestBody> {
    let mut incoming = body.into_data_stream();
    let mut chunks = Vec::new();
    let mut buffered = 0usize;
    while let Some(chunk) = incoming.next().await {
        let chunk = chunk.map_err(|error| anyhow!(error))?;
        buffered = buffered.saturating_add(chunk.len());
        chunks.push(chunk);
        if buffered > buffer_limit {
            let probe = join_chunks(&chunks, buffer_limit);
            let prefix =
                futures_util::stream::iter(chunks.into_iter().map(Ok::<Bytes, axum::Error>));
            let stream: IncomingBodyStream = Box::pin(prefix.chain(incoming));
            return Ok(RequestBody::Streaming {
                stream: Arc::new(Mutex::new(Some(stream))),
                probe,
            });
        }
    }
    Ok(RequestBody::Buffered(join_chunks(&chunks, buffered)))
}

fn join_chunks(chunks: &[Bytes], limit: usize) -> Bytes {
    let length = chunks.iter().map(Bytes::len).sum::<usize>().min(limit);
    if length == 0 {
        return Bytes::new();
    }
    let mut body = Vec::with_capacity(length);
    for chunk in chunks {
        let remaining = length.saturating_sub(body.len());
        if remaining == 0 {
            break;
        }
        body.extend_from_slice(&chunk[..chunk.len().min(remaining)]);
    }
    Bytes::from(body)
}

#[derive(Clone)]
struct PreparedRequest {
    method: Method,
    uri: Uri,
    headers: HeaderMap,
    body: RequestBody,
    provider: Provider,
    agent: String,
    session_id: String,
    user: String,
    model: String,
    account: Account,
    broker_lease: Option<broker::Lease>,
    bound_lease: Option<Lease>,
    disable_failover: bool,
    remote: Option<SocketAddr>,
}

enum ProxyBody {
    Buffered(Bytes),
    Streaming(reqwest::Response),
}

struct ProxyResult {
    status: StatusCode,
    headers: HeaderMap,
    body: ProxyBody,
    account: Account,
    prepared: PreparedRequest,
}

async fn execute_buffered(server: Arc<Server>, prepared: PreparedRequest) -> BufferedResponse {
    match execute_upstream(Arc::clone(&server), prepared.clone()).await {
        Ok(result) => {
            let mut status = result.status;
            let mut headers = result.headers;
            let body = match result.body {
                ProxyBody::Buffered(body) => body,
                ProxyBody::Streaming(response) => match response.bytes().await {
                    Ok(body) => body,
                    Err(error) => {
                        error!(error = %error, "buffer coalesced upstream response");
                        return BufferedResponse {
                            status: StatusCode::BAD_GATEWAY.as_u16(),
                            headers: text_headers(),
                            body: Bytes::from_static(b"upstream request failed\n"),
                        };
                    }
                },
            };
            let target = target_url(&server.upstreams, &result.prepared.uri, &result.account).ok();
            let mut body = body;
            if status.is_success()
                && catalog::is_catalog_list_path(result.prepared.uri.path())
                && let Some(target) = target
            {
                let auth_headers = authenticated_headers(&result.prepared.headers, &result.account);
                match catalog::aggregate_catalog_pages(
                    &server.client,
                    &result.prepared.method,
                    &result.prepared.uri,
                    &target,
                    &auth_headers,
                    body.clone(),
                )
                .await
                {
                    Ok(aggregate) => {
                        if aggregate.aggregated {
                            info!(pages = aggregate.pages, entries = aggregate.entries, path = %result.prepared.uri.path(), "aggregated plugin catalog pages");
                        }
                        body = aggregate.body;
                    }
                    Err(error) => {
                        warn!(error = %error, path = %result.prepared.uri.path(), "catalog aggregation failed");
                        status = StatusCode::BAD_GATEWAY;
                        headers = text_headers();
                        body = Bytes::from_static(b"catalog aggregation failed upstream\n");
                    }
                }
            }
            headers.remove(http::header::CONTENT_LENGTH);
            BufferedResponse {
                status: status.as_u16(),
                headers,
                body,
            }
        }
        Err(error) => {
            error!(error = %error, "coalesced proxy request failed");
            BufferedResponse {
                status: StatusCode::BAD_GATEWAY.as_u16(),
                headers: text_headers(),
                body: Bytes::from_static(b"upstream request failed\n"),
            }
        }
    }
}

async fn execute_upstream(
    server: Arc<Server>,
    mut prepared: PreparedRequest,
) -> anyhow::Result<ProxyResult> {
    let replayable = prepared.body.is_buffered()
        && retryable_request(prepared.provider, &prepared.method, prepared.uri.path());
    let _upload_permit = if replayable {
        Some(Arc::clone(&server.upload_limiter).acquire_owned().await?)
    } else {
        None
    };
    record_request(&server, &prepared);
    let mut tried = HashSet::from([prepared.account.id.clone()]);
    let account_attempt_limit = filter_accounts_for_provider(server.accounts(), prepared.provider)
        .len()
        .max(MAX_REPLAY_ATTEMPTS);
    let mut account_attempt = 0;
    let mut overload_retries = 0;
    loop {
        account_attempt += 1;
        let target = target_url(&server.upstreams, &prepared.uri, &prepared.account)?;
        let headers = authenticated_headers(&prepared.headers, &prepared.account);
        let mut transport_attempt = 0;
        let response = loop {
            transport_attempt += 1;
            info!(
                agent = %prepared.agent,
                session = %prepared.session_id,
                user = %prepared.user,
                account = %prepared.account.id,
                method = %prepared.method,
                path = %prepared.uri.path(),
                upstream = %target.host_str().unwrap_or_default(),
                remote_addr = %prepared.remote.map_or_else(String::new, |value| value.ip().to_string()),
                "proxy request"
            );
            let body = request_body_for_attempt(&server, &prepared)?;
            match send_request(
                &server.client,
                &prepared.method,
                target.clone(),
                &headers,
                body,
            )
            .await
            {
                Ok(response) => {
                    if replayable
                        && response.status() == StatusCode::REQUEST_TIMEOUT
                        && transport_attempt < MAX_REPLAY_ATTEMPTS
                    {
                        tokio::time::sleep(retry_backoff(transport_attempt)).await;
                        continue;
                    }
                    break response;
                }
                Err(error)
                    if replayable
                        && retryable_transport_error(&error)
                        && transport_attempt < MAX_REPLAY_ATTEMPTS =>
                {
                    warn!(attempt = transport_attempt + 1, error = %error, "retrying replayable upstream request after transport failure");
                    tokio::time::sleep(retry_backoff(transport_attempt)).await;
                }
                Err(error) => return Err(error.into()),
            }
        };
        let status = response.status();
        let response_headers = response.headers().clone();
        let mut response = Some(response);
        if prepared.provider == Provider::Claude
            && status.is_server_error()
            && !claude_rejected(&response_headers)
            && overload_retries < 2
            && replayable
        {
            let wait = claude_overload_backoff(&response_headers, overload_retries);
            overload_retries += 1;
            warn!(status = status.as_u16(), wait_ms = wait.as_millis(), account = %prepared.account.id, "retrying after anthropic overload");
            drop(response.take());
            tokio::time::sleep(wait).await;
            continue;
        }
        let should_inspect = prepared.provider == Provider::Claude
            && (matches!(
                status,
                StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN | StatusCode::TOO_MANY_REQUESTS
            ) || claude_rejected(&response_headers))
            || prepared.provider == Provider::Codex && status.is_client_error();
        let mut buffered = None;
        let mut usage_limited = false;
        let mut model_unsupported = false;
        if should_inspect {
            let body = response
                .take()
                .expect("response available before inspection")
                .bytes()
                .await?;
            let inspect = &body[..body.len().min(USAGE_INSPECT_MAX_BYTES)];
            usage_limited = if prepared.provider == Provider::Claude {
                matches!(
                    status,
                    StatusCode::UNAUTHORIZED
                        | StatusCode::FORBIDDEN
                        | StatusCode::TOO_MANY_REQUESTS
                ) || claude_rejected(&response_headers)
            } else {
                usage_limit_json(inspect)
            };
            model_unsupported = prepared.provider == Provider::Codex
                && status == StatusCode::BAD_REQUEST
                && model_unsupported_json(inspect);
            buffered = Some(body);
        }
        if let Some(lease) = &prepared.broker_lease {
            report_broker_lease(
                &server,
                lease,
                prepared.account.provider,
                prepared.account.auth_mode,
                status,
                &response_headers,
            )
            .await;
        }
        let local_failover = !prepared.disable_failover
            && prepared.broker_lease.is_none()
            && prepared.bound_lease.is_none()
            && replayable;
        if local_failover
            && (usage_limited || model_unsupported)
            && account_attempt < account_attempt_limit
        {
            let pool = if prepared.provider == Provider::Claude {
                claude_pool_model(&prepared.model)
            } else {
                prepared.model.clone()
            };
            if model_unsupported {
                self_mark_model_incompatible(&server, &prepared.account, &pool);
            } else if prepared.provider == Provider::Codex
                || claude_exhausted(status, &response_headers)
            {
                mark_exhausted_from_response(
                    &server,
                    &prepared.account,
                    &pool,
                    status,
                    &response_headers,
                );
            }
            match server
                .alternate_account(
                    prepared.provider,
                    &prepared.agent,
                    &prepared.session_id,
                    &prepared.user,
                    &pool,
                    &tried,
                    prepared.provider == Provider::Claude && server.claude_fallback_enabled(),
                )
                .await
            {
                Ok(next) => {
                    warn!(previous_account = %prepared.account.id, account = %next.id, attempt = account_attempt + 1, "retrying replayable upstream request after account rejection");
                    tried.insert(next.id.clone());
                    prepared.account = next;
                    server
                        .scheduler
                        .note_routed(prepared.provider, &prepared.account.id);
                    overload_retries = 0;
                    continue;
                }
                Err(error) => {
                    warn!(error = %error, account = %prepared.account.id, "usage-limit retry has no alternate account");
                    if let Some(response) = try_fable_fallback_raw(&server, &prepared).await {
                        return response;
                    }
                }
            }
        }
        server
            .scheduler
            .note_routed(prepared.provider, &prepared.account.id);
        return Ok(ProxyResult {
            status,
            headers: response_headers,
            body: buffered.map_or_else(
                || ProxyBody::Streaming(response.expect("unbuffered response remains available")),
                ProxyBody::Buffered,
            ),
            account: prepared.account.clone(),
            prepared,
        });
    }
}

fn request_body_for_attempt(
    server: &Server,
    prepared: &PreparedRequest,
) -> anyhow::Result<reqwest::Body> {
    match &prepared.body {
        RequestBody::Buffered(body) => Ok(reqwest::Body::from(body.clone())),
        RequestBody::Streaming { .. } => {
            let stream = RecordedRequestStream {
                inner: prepared.body.take_stream()?,
                recorder: server.transcripts.clone(),
                agent: prepared.agent.clone(),
                session_id: prepared.session_id.clone(),
                account: prepared.account.id.clone(),
                stream_id: format!(
                    "body-{}",
                    Utc::now().timestamp_nanos_opt().unwrap_or_default()
                ),
                chunks: 0,
                bytes: 0,
                digest: Sha256::new(),
                finished: false,
            };
            Ok(reqwest::Body::wrap_stream(stream))
        }
    }
}

async fn send_request(
    client: &Client,
    method: &Method,
    target: Url,
    headers: &HeaderMap,
    body: reqwest::Body,
) -> reqwest::Result<reqwest::Response> {
    let mut request = client.request(method.clone(), target);
    for (name, value) in headers {
        if !is_hop_by_hop(name.as_str())
            && *name != http::header::HOST
            && *name != http::header::CONTENT_LENGTH
        {
            request = request.header(name, value);
        }
    }
    request.body(body).send().await
}

fn proxy_result_into_response(
    server: Arc<Server>,
    result: ProxyResult,
    lifecycle: LifecycleGuard,
    active: Option<ActiveGuard>,
) -> Response {
    let mut builder = Response::builder().status(result.status);
    copy_response_headers(
        builder.headers_mut().expect("response builder headers"),
        &result.headers,
    );
    let body = match result.body {
        ProxyBody::Buffered(body) => {
            record_response_body(&server, &result.prepared, &result.account, &body);
            drop((lifecycle, active));
            Body::from(body)
        }
        ProxyBody::Streaming(response) => {
            let stream = RecordedResponseStream {
                inner: Box::pin(response.bytes_stream()),
                recorder: server.transcripts.clone(),
                agent: result.prepared.agent.clone(),
                session_id: result.prepared.session_id.clone(),
                account: result.account.id.clone(),
                stream_id: format!(
                    "body-{}",
                    Utc::now().timestamp_nanos_opt().unwrap_or_default()
                ),
                chunks: 0,
                bytes: 0,
                digest: Sha256::new(),
                finished: false,
            };
            let guarded = GuardedStream {
                inner: Box::pin(stream),
                _lifecycle: Some(lifecycle),
                _active: active,
            };
            Body::from_stream(guarded)
        }
    };
    builder.body(body).unwrap_or_else(|error| {
        text_response(
            StatusCode::INTERNAL_SERVER_ERROR,
            &format!("response build failed: {error}\n"),
        )
    })
}

struct RecordedRequestStream {
    inner: IncomingBodyStream,
    recorder: Option<Arc<Recorder>>,
    agent: String,
    session_id: String,
    account: String,
    stream_id: String,
    chunks: usize,
    bytes: u64,
    digest: Sha256,
    finished: bool,
}

impl RecordedRequestStream {
    fn finish(&mut self) {
        if self.finished {
            return;
        }
        self.finished = true;
        if let Some(recorder) = &self.recorder {
            recorder.record_payload_summary(
                &self.agent,
                &self.session_id,
                "http_body",
                "client_to_upstream",
                &self.stream_id,
                self.bytes,
                &hex::encode(self.digest.clone().finalize()),
                self.chunks,
                Map::from_iter([("account".into(), self.account.clone().into())]),
            );
        }
    }
}

impl Stream for RecordedRequestStream {
    type Item = Result<Bytes, axum::Error>;

    fn poll_next(
        mut self: Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        match self.inner.as_mut().poll_next(context) {
            std::task::Poll::Ready(Some(Ok(bytes))) => {
                if let Some(recorder) = &self.recorder {
                    recorder.record_payload_chunk(
                        &self.agent,
                        &self.session_id,
                        "http_body",
                        "client_to_upstream",
                        &self.stream_id,
                        self.chunks,
                        self.bytes,
                        &bytes,
                        Map::from_iter([("account".into(), self.account.clone().into())]),
                    );
                }
                self.digest.update(&bytes);
                self.chunks += 1;
                self.bytes += bytes.len() as u64;
                std::task::Poll::Ready(Some(Ok(bytes)))
            }
            std::task::Poll::Ready(Some(Err(error))) => {
                error!(error = %error, agent = %self.agent, session = %self.session_id, account = %self.account, "proxy request stream read failed");
                std::task::Poll::Ready(Some(Err(error)))
            }
            std::task::Poll::Ready(None) => {
                self.finish();
                std::task::Poll::Ready(None)
            }
            std::task::Poll::Pending => std::task::Poll::Pending,
        }
    }
}

struct RecordedResponseStream<S> {
    inner: std::pin::Pin<Box<S>>,
    recorder: Option<Arc<Recorder>>,
    agent: String,
    session_id: String,
    account: String,
    stream_id: String,
    chunks: usize,
    bytes: u64,
    digest: Sha256,
    finished: bool,
}

impl<S> RecordedResponseStream<S> {
    fn finish(&mut self) {
        if self.finished {
            return;
        }
        self.finished = true;
        if let Some(recorder) = &self.recorder {
            recorder.record_payload_summary(
                &self.agent,
                &self.session_id,
                "http_body",
                "upstream_to_client",
                &self.stream_id,
                self.bytes,
                &hex::encode(self.digest.clone().finalize()),
                self.chunks,
                Map::from_iter([("account".into(), self.account.clone().into())]),
            );
        }
    }
}

impl<S> futures_util::Stream for RecordedResponseStream<S>
where
    S: futures_util::Stream<Item = Result<Bytes, reqwest::Error>>,
{
    type Item = Result<Bytes, reqwest::Error>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        match self.inner.as_mut().poll_next(context) {
            std::task::Poll::Ready(Some(Ok(bytes))) => {
                if let Some(recorder) = &self.recorder {
                    recorder.record_payload_chunk(
                        &self.agent,
                        &self.session_id,
                        "http_body",
                        "upstream_to_client",
                        &self.stream_id,
                        self.chunks,
                        self.bytes,
                        &bytes,
                        Map::from_iter([("account".into(), self.account.clone().into())]),
                    );
                }
                self.digest.update(&bytes);
                self.chunks += 1;
                self.bytes += bytes.len() as u64;
                std::task::Poll::Ready(Some(Ok(bytes)))
            }
            std::task::Poll::Ready(Some(Err(error))) => {
                error!(error = %error, agent = %self.agent, session = %self.session_id, account = %self.account, "proxy response stream read failed");
                std::task::Poll::Ready(Some(Err(error)))
            }
            std::task::Poll::Ready(None) => {
                self.finish();
                std::task::Poll::Ready(None)
            }
            std::task::Poll::Pending => std::task::Poll::Pending,
        }
    }
}

struct GuardedStream<S> {
    inner: std::pin::Pin<Box<S>>,
    _lifecycle: Option<LifecycleGuard>,
    _active: Option<ActiveGuard>,
}

impl<S> futures_util::Stream for GuardedStream<S>
where
    S: futures_util::Stream<Item = Result<Bytes, reqwest::Error>>,
{
    type Item = Result<Bytes, reqwest::Error>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        self.inner.as_mut().poll_next(context)
    }
}

async fn proxy_websocket(
    server: Arc<Server>,
    upgrade: WebSocketUpgrade,
    prepared: PreparedRequest,
    lifecycle: LifecycleGuard,
    active: Option<ActiveGuard>,
) -> Response {
    let target = match target_url(&server.upstreams, &prepared.uri, &prepared.account) {
        Ok(mut target) => {
            let _ = target.set_scheme(if target.scheme() == "https" {
                "wss"
            } else {
                "ws"
            });
            target
        }
        Err(error) => return text_response(StatusCode::SERVICE_UNAVAILABLE, &format!("{error}\n")),
    };
    let mut request = match target.as_str().into_client_request() {
        Ok(request) => request,
        Err(error) => return text_response(StatusCode::BAD_GATEWAY, &format!("{error}\n")),
    };
    let headers = authenticated_headers(&prepared.headers, &prepared.account);
    for (name, value) in &headers {
        if !is_websocket_handshake_header(name.as_str())
            && !is_hop_by_hop(name.as_str())
            && *name != http::header::HOST
        {
            request.headers_mut().append(name, value.clone());
        }
    }
    let (upstream, handshake) = match tokio_tungstenite::connect_async(request).await {
        Ok(value) => value,
        Err(WebSocketError::Http(response)) => {
            error!(status = response.status().as_u16(), path = %prepared.uri.path(), account = %prepared.account.id, "websocket upstream dial failed");
            return text_response(
                response.status(),
                "websocket upstream rejected the connection\n",
            );
        }
        Err(error) => {
            error!(error = %error, path = %prepared.uri.path(), account = %prepared.account.id, "websocket upstream dial failed");
            return text_response(StatusCode::BAD_GATEWAY, &format!("{error}\n"));
        }
    };
    if let Some(lease) = &prepared.broker_lease {
        report_broker_lease(
            &server,
            lease,
            prepared.account.provider,
            prepared.account.auth_mode,
            StatusCode::SWITCHING_PROTOCOLS,
            handshake.headers(),
        )
        .await;
    }
    let response = upgrade.on_upgrade(move |client| async move {
        bridge_websocket(server, client, upstream, prepared).await;
        drop((lifecycle, active));
    });
    response.into_response()
}

async fn bridge_websocket(
    server: Arc<Server>,
    client: WebSocket,
    upstream: tokio_tungstenite::WebSocketStream<
        tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
    >,
    prepared: PreparedRequest,
) {
    let (mut client_sink, mut client_stream) = client.split();
    let (mut upstream_sink, mut upstream_stream) = upstream.split();
    let client_to_upstream = async {
        while let Some(message) = client_stream.next().await {
            let message = message?;
            record_websocket(&server, &prepared, "client_to_upstream", &message);
            if let Some(message) = axum_to_tungstenite(message) {
                upstream_sink
                    .send(message)
                    .await
                    .map_err(|error| anyhow!(error))?;
            }
        }
        anyhow::Ok(())
    };
    let upstream_to_client = async {
        while let Some(message) = upstream_stream.next().await {
            let message = message?;
            if let Some(message) = tungstenite_to_axum(message) {
                record_websocket(&server, &prepared, "upstream_to_client", &message);
                client_sink
                    .send(message)
                    .await
                    .map_err(|error| anyhow!(error))?;
            }
        }
        anyhow::Ok(())
    };
    let result = tokio::select! {
        result = client_to_upstream => result,
        result = upstream_to_client => result,
    };
    if let Err(error) = result {
        debug!(error = %error, agent = %prepared.agent, session = %prepared.session_id, "websocket relay ended");
    }
}

fn axum_to_tungstenite(message: AxumMessage) -> Option<TungsteniteMessage> {
    match message {
        AxumMessage::Text(value) => Some(TungsteniteMessage::Text(value.to_string().into())),
        AxumMessage::Binary(value) => Some(TungsteniteMessage::Binary(value)),
        AxumMessage::Ping(value) => Some(TungsteniteMessage::Ping(value)),
        AxumMessage::Pong(value) => Some(TungsteniteMessage::Pong(value)),
        AxumMessage::Close(_) => Some(TungsteniteMessage::Close(None)),
    }
}

fn tungstenite_to_axum(message: TungsteniteMessage) -> Option<AxumMessage> {
    match message {
        TungsteniteMessage::Text(value) => Some(AxumMessage::Text(value.to_string().into())),
        TungsteniteMessage::Binary(value) => Some(AxumMessage::Binary(value)),
        TungsteniteMessage::Ping(value) => Some(AxumMessage::Ping(value)),
        TungsteniteMessage::Pong(value) => Some(AxumMessage::Pong(value)),
        TungsteniteMessage::Close(_) => Some(AxumMessage::Close(None)),
        TungsteniteMessage::Frame(_) => None,
    }
}

fn record_websocket(
    server: &Server,
    prepared: &PreparedRequest,
    direction: &str,
    message: &AxumMessage,
) {
    let Some(recorder) = &server.transcripts else {
        return;
    };
    let body = match message {
        AxumMessage::Text(value) => value.as_bytes(),
        AxumMessage::Binary(value) | AxumMessage::Ping(value) | AxumMessage::Pong(value) => value,
        AxumMessage::Close(_) => &[],
    };
    recorder.record_payload(
        &prepared.agent,
        &prepared.session_id,
        "websocket_message",
        direction,
        body,
        Map::from_iter([
            ("account".into(), prepared.account.id.clone().into()),
            ("user".into(), prepared.user.clone().into()),
        ]),
    );
}

fn record_request(server: &Server, prepared: &PreparedRequest) {
    let Some(recorder) = &server.transcripts else {
        return;
    };
    let mut payload = Map::new();
    payload.insert("method".into(), prepared.method.to_string().into());
    payload.insert("path".into(), prepared.uri.path().into());
    payload.insert("account".into(), prepared.account.id.clone().into());
    payload.insert("provider".into(), prepared.provider.to_string().into());
    payload.insert("user".into(), prepared.user.clone().into());
    payload.insert(
        "headers".into(),
        serde_json::to_value(transcript::redacted_headers(&prepared.headers)).unwrap_or_default(),
    );
    recorder.record_meta(&prepared.agent, &prepared.session_id, payload.clone());
    if let Some(body) = prepared.body.buffered().filter(|body| !body.is_empty()) {
        recorder.record_payload(
            &prepared.agent,
            &prepared.session_id,
            "http_body",
            "client_to_upstream",
            body,
            payload,
        );
    }
}

fn record_response_body(
    server: &Server,
    prepared: &PreparedRequest,
    account: &Account,
    body: &[u8],
) {
    let Some(recorder) = &server.transcripts else {
        return;
    };
    recorder.record_payload(
        &prepared.agent,
        &prepared.session_id,
        "http_body",
        "upstream_to_client",
        body,
        Map::from_iter([
            ("account".into(), account.id.clone().into()),
            ("user".into(), prepared.user.clone().into()),
        ]),
    );
}

async fn try_fable_fallback(
    server: &Server,
    provider: Provider,
    model: &str,
    parts: &http::request::Parts,
    body: Bytes,
) -> Option<Response> {
    if provider != Provider::Claude || !claude_fable_model(model) {
        return None;
    }
    if let Some(config) = server.bedrock.as_ref().filter(|config| config.configured()) {
        match config.fable_response(&body).await {
            Ok(response)
                if response.status.is_success()
                    || server.claude_fable_api_key.trim().is_empty() =>
            {
                return Some(response.into_axum());
            }
            Ok(response) => warn!(
                status = response.status.as_u16(),
                "Fable Bedrock fallback unusable, trying dedicated API key"
            ),
            Err(error) if server.claude_fable_api_key.trim().is_empty() => {
                warn!(%error, "Fable Bedrock fallback failed");
                return None;
            }
            Err(error) => warn!(%error, "Fable Bedrock fallback failed, trying dedicated API key"),
        }
    }
    if server.claude_fable_api_key.trim().is_empty() {
        return None;
    }
    let body = strip_claude_unsupported_fields(body);
    let prepared = PreparedRequest {
        method: parts.method.clone(),
        uri: parts.uri.clone(),
        headers: parts.headers.clone(),
        body: RequestBody::Buffered(body),
        provider,
        agent: "claude".into(),
        session_id: String::new(),
        user: String::new(),
        model: model.into(),
        account: Account {
            id: "fable-api-key".into(),
            provider: Provider::Claude,
            auth_mode: AuthMode::ApiKey,
            label: "fable-api-key".into(),
            token: server.claude_fable_api_key.clone(),
            ..Account::default()
        },
        broker_lease: None,
        bound_lease: None,
        disable_failover: true,
        remote: None,
    };
    let server = Arc::new(server.clone());
    execute_upstream(Arc::clone(&server), prepared)
        .await
        .ok()
        .map(|result| {
            proxy_result_into_response(
                server,
                result,
                LifecycleGuard(Arc::new(Lifecycle::default())),
                None,
            )
        })
}

async fn try_fable_fallback_raw(
    server: &Arc<Server>,
    prepared: &PreparedRequest,
) -> Option<anyhow::Result<ProxyResult>> {
    if prepared.provider != Provider::Claude || !claude_fable_model(&prepared.model) {
        return None;
    }
    if let Some(config) = server.bedrock.as_ref().filter(|config| config.configured()) {
        let body = prepared.body.buffered()?;
        match config.fable_response(body).await {
            Ok(response)
                if response.status.is_success()
                    || server.claude_fable_api_key.trim().is_empty() =>
            {
                let account = Account {
                    id: "fable-bedrock".into(),
                    provider: Provider::Claude,
                    auth_mode: AuthMode::ApiKey,
                    label: format!("AWS Bedrock {}", response.source),
                    ..Account::default()
                };
                return Some(Ok(ProxyResult {
                    status: response.status,
                    headers: response.headers,
                    body: ProxyBody::Buffered(response.body),
                    account,
                    prepared: prepared.clone(),
                }));
            }
            Ok(response) => warn!(
                status = response.status.as_u16(),
                "Fable Bedrock fallback unusable, trying dedicated API key"
            ),
            Err(error) if server.claude_fable_api_key.trim().is_empty() => return Some(Err(error)),
            Err(error) => warn!(%error, "Fable Bedrock fallback failed, trying dedicated API key"),
        }
    }
    if server.claude_fable_api_key.trim().is_empty() {
        return None;
    }
    let mut fallback = prepared.clone();
    fallback.account = Account {
        id: "fable-api-key".into(),
        provider: Provider::Claude,
        auth_mode: AuthMode::ApiKey,
        label: "fable-api-key".into(),
        token: server.claude_fable_api_key.clone(),
        ..Account::default()
    };
    let body = fallback.body.buffered()?.clone();
    fallback.body = RequestBody::Buffered(strip_claude_unsupported_fields(body));
    fallback.disable_failover = true;
    Some(Box::pin(execute_upstream(Arc::clone(server), fallback)).await)
}

fn strip_claude_unsupported_fields(body: Bytes) -> Bytes {
    let Ok(mut payload) = serde_json::from_slice::<Map<String, Value>>(&body) else {
        return body;
    };
    if payload.remove("context_management").is_none() {
        return body;
    }
    serde_json::to_vec(&payload)
        .map(Bytes::from)
        .unwrap_or(body)
}

fn authenticated_headers(source: &HeaderMap, account: &Account) -> HeaderMap {
    let mut headers = source.clone();
    strip_internal_headers(&mut headers);
    strip_forwarding_headers(&mut headers);
    headers.remove(http::header::AUTHORIZATION);
    headers.remove("x-api-key");
    headers.remove("chatgpt-account-id");
    match account.provider {
        Provider::Claude => {
            if account.auth_mode == AuthMode::ApiKey {
                insert_header(&mut headers, "x-api-key", &account.token);
                remove_comma_header_value(&mut headers, "anthropic-beta", CLAUDE_OAUTH_BETA_HEADER);
            } else {
                insert_header(
                    &mut headers,
                    "authorization",
                    &account.authorization_header(),
                );
                ensure_comma_header_value(&mut headers, "anthropic-beta", CLAUDE_OAUTH_BETA_HEADER);
            }
        }
        Provider::Kimi => {
            insert_header(
                &mut headers,
                "authorization",
                &account.authorization_header(),
            );
            insert_header(&mut headers, "x-api-key", &account.token);
            if !headers.contains_key("anthropic-version") {
                headers.insert("anthropic-version", HeaderValue::from_static("2023-06-01"));
            }
        }
        Provider::Zai => insert_header(
            &mut headers,
            "authorization",
            &account.authorization_header(),
        ),
        Provider::Codex => {
            insert_header(
                &mut headers,
                "authorization",
                &account.authorization_header(),
            );
            if !account.account_id.is_empty() {
                insert_header(&mut headers, "chatgpt-account-id", &account.account_id);
            }
        }
    }
    headers
}

fn target_url(upstreams: &Upstreams, uri: &Uri, account: &Account) -> anyhow::Result<Url> {
    let (mut base, rewritten) = if let Some(override_upstream) = &upstreams.override_upstream {
        (override_upstream.clone(), uri.path().to_owned())
    } else {
        match account.provider {
            Provider::Claude => (upstreams.claude.clone(), uri.path().to_owned()),
            Provider::Kimi => {
                let mut path = strip_provider_prefix(uri.path(), "kimi");
                if base_path_ends_v1(upstreams.kimi.path()) {
                    path = strip_v1_prefix(&path);
                }
                (upstreams.kimi.clone(), path)
            }
            Provider::Zai => (
                upstreams.zai.clone(),
                strip_provider_prefix(uri.path(), "zai"),
            ),
            Provider::Codex if account.auth_mode == AuthMode::ApiKey => {
                let path = if uri.path() == "/v1" || uri.path().starts_with("/v1/") {
                    uri.path().to_owned()
                } else {
                    format!("/v1{}", uri.path())
                };
                (upstreams.api.clone(), path)
            }
            Provider::Codex if chatgpt_backend_path(uri.path()) => {
                let mut base = upstreams.codex.clone();
                if base
                    .path()
                    .trim_end_matches('/')
                    .ends_with("/backend-api/codex")
                {
                    let path = base
                        .path()
                        .trim_end_matches('/')
                        .trim_end_matches("/codex")
                        .to_owned();
                    base.set_path(&path);
                }
                (
                    base,
                    catalog::strip_chatgpt_backend_path(uri.path())
                        .unwrap_or(uri.path())
                        .to_owned(),
                )
            }
            Provider::Codex => (upstreams.codex.clone(), strip_v1_prefix(uri.path())),
        }
    };
    let prefix = base.path().trim_end_matches('/');
    let path = if prefix.is_empty() {
        rewritten
    } else {
        format!("{prefix}/{}", rewritten.trim_start_matches('/'))
    };
    base.set_path(&path);
    base.set_query(uri.query());
    base.set_fragment(None);
    Ok(base)
}

fn strip_v1_prefix(path: &str) -> String {
    if path == "/v1" {
        "/".into()
    } else {
        path.strip_prefix("/v1/")
            .map_or_else(|| path.to_owned(), |value| format!("/{value}"))
    }
}

fn base_path_ends_v1(path: &str) -> bool {
    path.trim_end_matches('/').ends_with("/v1")
}

fn strip_provider_prefix(path: &str, provider: &str) -> String {
    let prefix = format!("/{provider}");
    if path == prefix {
        "/".into()
    } else {
        path.strip_prefix(&format!("{prefix}/"))
            .map_or_else(|| path.to_owned(), |value| format!("/{value}"))
    }
}

fn provider_for_request(agent: &str, path: &str) -> Provider {
    match path
        .trim_start_matches('/')
        .split('/')
        .next()
        .unwrap_or_default()
    {
        "kimi" => Provider::Kimi,
        "zai" => Provider::Zai,
        _ if session::normalize_agent_type(agent).as_deref() == Some("claude") => Provider::Claude,
        _ => Provider::Codex,
    }
}

fn agent_for_provider_session(agent: &str, provider: Provider) -> String {
    match provider {
        Provider::Kimi | Provider::Zai => provider.to_string(),
        Provider::Codex | Provider::Claude => agent.into(),
    }
}

fn filter_accounts_for_provider(accounts: Vec<Account>, provider: Provider) -> Vec<Account> {
    accounts
        .into_iter()
        .filter(|account| account.provider == provider)
        .collect()
}

fn account_matches(account: &Account, value: &str) -> bool {
    let value = value.trim();
    !value.is_empty()
        && (account.id.eq_ignore_ascii_case(value)
            || account.label.eq_ignore_ascii_case(value)
            || account.email.eq_ignore_ascii_case(value)
            || account.auth_mode == AuthMode::ApiKey
                && account
                    .id
                    .trim_start_matches("apikey:")
                    .eq_ignore_ascii_case(value))
}

fn chatgpt_backend_path(path: &str) -> bool {
    path == "/backend-api" || path.starts_with("/backend-api/")
}

fn claude_fable_model(model: &str) -> bool {
    selectacct::model_key(model) == selectacct::model_key(claude::FABLE_MODEL)
}

fn claude_pool_model(model: &str) -> String {
    if claude_fable_model(model) {
        claude::FABLE_FEATURE.into()
    } else {
        model.into()
    }
}

fn active_session_request(agent: &str, method: &Method, path: &str, websocket: bool) -> bool {
    websocket
        || session::normalize_agent_type(agent).as_deref() == Some("codex")
            && *method == Method::POST
            && matches!(
                path,
                "/responses"
                    | "/v1/responses"
                    | "/responses/compact"
                    | "/v1/responses/compact"
                    | "/backend-api/codex/responses"
                    | "/backend-api/codex/responses/compact"
            )
}

fn retryable_request(provider: Provider, method: &Method, path: &str) -> bool {
    if *method != Method::POST {
        return false;
    }
    match provider {
        Provider::Claude => matches!(path, "/v1/messages" | "/messages"),
        Provider::Codex => matches!(
            path,
            "/responses"
                | "/v1/responses"
                | "/responses/compact"
                | "/v1/responses/compact"
                | "/alpha/search"
                | "/v1/alpha/search"
        ),
        Provider::Kimi | Provider::Zai => false,
    }
}

fn retry_backoff(attempt: usize) -> Duration {
    Duration::from_millis((100u64 << attempt.saturating_sub(1)).min(800))
}

fn retryable_transport_error(error: &reqwest::Error) -> bool {
    let message = error.to_string().to_ascii_lowercase();
    [
        "broken pipe",
        "closed network connection",
        "bad record mac",
        "connection reset",
        "unexpected eof",
    ]
    .iter()
    .any(|marker| message.contains(marker))
}

fn usage_limit_json(body: &[u8]) -> bool {
    fn matches(value: &Value) -> bool {
        let field = |name| {
            value
                .get(name)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_ascii_lowercase()
        };
        if [field("type"), field("code")]
            .iter()
            .any(|value| value == "usage_limit_reached")
        {
            return true;
        }
        let message = field("message");
        if message.contains("usage limit")
            && ["reached", "hit", "exceeded"]
                .iter()
                .any(|word| message.contains(word))
        {
            return true;
        }
        match value.get("error") {
            Some(Value::Object(_)) => matches(&value["error"]),
            Some(Value::String(message)) => {
                let message = message.to_ascii_lowercase();
                message.contains("usage limit")
                    && ["reached", "hit", "exceeded"]
                        .iter()
                        .any(|word| message.contains(word))
            }
            _ => false,
        }
    }
    serde_json::from_slice::<Value>(body).is_ok_and(|value| matches(&value))
}

fn model_unsupported_json(body: &[u8]) -> bool {
    fn matches(value: &Value) -> bool {
        let message = value
            .get("message")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_ascii_lowercase();
        (message.contains("model is not supported when using codex with a chatgpt account")
            || message.contains("model")
                && message.contains("not supported")
                && message.contains("codex")
                && message.contains("chatgpt account"))
            || value.get("error").is_some_and(matches)
    }
    serde_json::from_slice::<Value>(body).is_ok_and(|value| matches(&value))
}

fn claude_rejected(headers: &HeaderMap) -> bool {
    header_str(headers, "anthropic-ratelimit-unified-status").eq_ignore_ascii_case("rejected")
}

fn claude_model_pool_rejected(headers: &HeaderMap) -> bool {
    header_str(headers, "anthropic-ratelimit-unified-7d_oi-status").eq_ignore_ascii_case("rejected")
        && !["5h", "7d"].iter().any(|window| {
            header_str(
                headers,
                &format!("anthropic-ratelimit-unified-{window}-status"),
            )
            .eq_ignore_ascii_case("rejected")
        })
}

fn claude_exhausted(status: StatusCode, headers: &HeaderMap) -> bool {
    matches!(status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN)
        || claude_rejected(headers) && !claude_model_pool_rejected(headers)
}

fn claude_overload_backoff(headers: &HeaderMap, retry: usize) -> Duration {
    header_str(headers, "retry-after")
        .parse::<u64>()
        .ok()
        .filter(|seconds| *seconds > 0)
        .map_or_else(
            || Duration::from_secs((1 << retry).min(10)),
            |seconds| Duration::from_secs(seconds.min(10)),
        )
}

fn mark_exhausted_from_response(
    server: &Server,
    account: &Account,
    pool: &str,
    status: StatusCode,
    headers: &HeaderMap,
) {
    if matches!(status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN) {
        server.scheduler.mark_exhausted_until(
            account.provider,
            &account.id,
            "",
            SystemTime::now() + CREDENTIAL_EXHAUSTION_TTL,
        );
        return;
    }
    let pool = if account.provider == Provider::Codex {
        ""
    } else {
        pool
    };
    let until = exhaustion_expiry(headers);
    server
        .scheduler
        .mark_exhausted_until(account.provider, &account.id, pool, until);
}

fn self_mark_model_incompatible(server: &Server, account: &Account, model: &str) {
    if !model.is_empty() {
        server
            .scheduler
            .mark_model_incompatible(account.provider, &account.id, model);
    }
}

fn exhaustion_expiry(headers: &HeaderMap) -> SystemTime {
    let now = SystemTime::now();
    let mut seconds = selectacct::DEFAULT_EXHAUSTED_TTL.as_secs();
    if let Ok(epoch) = header_str(headers, "anthropic-ratelimit-unified-reset").parse::<u64>() {
        let now_epoch = now.duration_since(UNIX_EPOCH).unwrap_or_default().as_secs();
        seconds = epoch.saturating_sub(now_epoch);
    } else if let Ok(retry_after) = header_str(headers, "retry-after").parse::<u64>() {
        seconds = retry_after;
    }
    now + Duration::from_secs(seconds.clamp(60, 8 * 24 * 60 * 60))
}

async fn report_broker_lease(
    server: &Server,
    lease: &broker::Lease,
    provider: Provider,
    auth_mode: AuthMode,
    status: StatusCode,
    headers: &HeaderMap,
) {
    let Some(broker) = &server.credential_broker else {
        return;
    };
    let outcome = if status == StatusCode::UNAUTHORIZED {
        broker::LeaseOutcome::Unauthorized
    } else if status == StatusCode::FORBIDDEN
        && !header_str(headers, "cf-mitigated").eq_ignore_ascii_case("challenge")
    {
        broker::LeaseOutcome::Forbidden
    } else if status == StatusCode::TOO_MANY_REQUESTS
        || provider == Provider::Claude && claude_rejected(headers)
    {
        broker::LeaseOutcome::RateLimited
    } else if status.is_success() || status.is_redirection() {
        broker::LeaseOutcome::Success
    } else {
        broker::LeaseOutcome::ProviderError
    };
    let cooldown_scope = match outcome {
        broker::LeaseOutcome::Forbidden
            if provider == Provider::Claude && auth_mode == AuthMode::Oauth =>
        {
            Some(broker::LeaseCooldownScope::Account)
        }
        broker::LeaseOutcome::Forbidden | broker::LeaseOutcome::RateLimited => {
            Some(broker::LeaseCooldownScope::Quota)
        }
        _ => None,
    };
    let retry_at = header_str(headers, "retry-after")
        .parse::<i64>()
        .ok()
        .filter(|seconds| *seconds > 0)
        .map(|seconds| Utc::now() + chrono::Duration::seconds(seconds.min(8 * 24 * 60 * 60)));
    let report = broker::LeaseReport {
        outcome,
        status_code: Some(status.as_u16()),
        cooldown_scope,
        retry_at,
    };
    if matches!(
        outcome,
        broker::LeaseOutcome::Unauthorized
            | broker::LeaseOutcome::Forbidden
            | broker::LeaseOutcome::RateLimited
    ) {
        broker.invalidate(&lease.id);
    }
    if let Err(error) = broker.report(&lease.id, &report).await {
        warn!(lease = %lease.id, status = status.as_u16(), error = %error, "credential lease report failed");
    }
}

fn update_scheduler_from_usage(server: &Server, statuses: &[AccountUsageStatus]) {
    if let Some(scheduler) = scheduler_from_usage(server, statuses) {
        server.scheduler.set(scheduler);
    }
}

fn scheduler_from_usage(server: &Server, statuses: &[AccountUsageStatus]) -> Option<Scheduler> {
    let available: Vec<_> = server
        .accounts()
        .into_iter()
        .filter(|account| account.auth_mode == AuthMode::Oauth)
        .collect();
    let mut scores = Vec::with_capacity(available.len());
    let mut indices = HashMap::new();
    for account in available {
        let mut score = server
            .scheduler
            .get()
            .score_for(account.provider, &account.id);
        score.account_id.clone_from(&account.id);
        score.provider = account.provider;
        score.fresh = false;
        indices.insert(
            selectacct::score_key(account.provider, &account.id),
            scores.len(),
        );
        scores.push(score);
    }
    let mut scored = 0;
    for status in statuses.iter().filter(|status| {
        status.fresh && status.account.auth_mode == AuthMode::Oauth && !status.windows.is_empty()
    }) {
        if let Some(index) = indices
            .get(&selectacct::score_key(
                status.account.provider,
                &status.account.id,
            ))
            .copied()
        {
            let windows: Vec<_> = status
                .windows
                .iter()
                .map(|window| selectacct::LimitWindow {
                    name: window.name.clone(),
                    used_percent: window.used_percent,
                    limit_window_seconds: window.limit_window_seconds,
                    reset_after_seconds: window.reset_after_seconds,
                    feature: window.feature.clone(),
                })
                .collect();
            let mut score = selectacct::score_from_limit_windows(&status.account.id, 0, &windows);
            score.provider = status.account.provider;
            score.fresh = true;
            scores[index] = score;
            scored += 1;
        }
    }
    (scored > 0)
        .then(|| Scheduler::new(scores).with_session_counts(server.sessions.count_by_account()))
}

fn refresh_scheduler_if_stale(server: Arc<Server>) {
    if !server
        .scheduler
        .begin_refresh_if_stale(server.usage_score_ttl)
    {
        return;
    }
    tokio::spawn(async move {
        let Some(reference) = &server.account_ref else {
            server
                .scheduler
                .finish_refresh(server.scheduler.get(), false);
            return;
        };
        let statuses = reference.usage_statuses().await;
        match scheduler_from_usage(&server, &statuses) {
            Some(scheduler) => server.scheduler.finish_refresh(scheduler, true),
            None => server
                .scheduler
                .finish_refresh(server.scheduler.get(), false),
        }
    });
}

fn request_for_session(parts: &http::request::Parts, body: Bytes) -> Request<Bytes> {
    let mut request = Request::builder()
        .method(parts.method.clone())
        .uri(parts.uri.clone())
        .body(body)
        .expect("valid copied request");
    *request.headers_mut() = parts.headers.clone();
    request
}

fn valid_http_base_url(value: &str) -> bool {
    Url::parse(value).is_ok_and(|url| {
        matches!(url.scheme(), "http" | "https")
            && url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.query().is_none()
            && url.fragment().is_none()
    })
}

fn lease_endpoint(provider: Provider) -> &'static str {
    match provider {
        Provider::Codex => "/backend-api/codex/responses",
        Provider::Claude => "/v1/messages",
        Provider::Kimi => "/kimi/v1/messages",
        Provider::Zai => "/zai/chat/completions",
    }
}

fn bearer_token(headers: &HeaderMap) -> Option<&str> {
    headers
        .get(http::header::AUTHORIZATION)?
        .to_str()
        .ok()?
        .trim()
        .split_once(' ')
        .filter(|(kind, _)| kind.eq_ignore_ascii_case("bearer"))
        .map(|(_, token)| token.trim())
}

fn constant_time_equal(left: &str, right: &str) -> bool {
    left.len() == right.len() && bool::from(left.as_bytes().ct_eq(right.as_bytes()))
}

fn strip_internal_headers(headers: &mut HeaderMap) {
    for name in [
        "x-subrouter-session",
        "x-subrouter-agent",
        "x-subrouter-user-email",
        "x-subrouter-user",
        "x-user-email",
        "x-subrouter-account-id",
        "x-subrouter-account",
        "x-subrouter-model",
        "x-subrouter-tenant-key",
        "x-tenant-key",
        "x-subrouter-lease",
    ] {
        headers.remove(name);
    }
}

fn strip_forwarding_headers(headers: &mut HeaderMap) {
    for name in [
        "forwarded",
        "x-forwarded-for",
        "x-forwarded-host",
        "x-forwarded-proto",
        "x-forwarded-ssl",
        "x-real-ip",
    ] {
        headers.remove(name);
    }
}

fn insert_header(headers: &mut HeaderMap, name: &'static str, value: &str) {
    if let Ok(value) = HeaderValue::from_str(value) {
        headers.insert(HeaderName::from_static(name), value);
    }
}

fn remove_comma_header_value(headers: &mut HeaderMap, name: &'static str, unwanted: &str) {
    let kept = header_str(headers, name)
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty() && *value != unwanted)
        .collect::<Vec<_>>()
        .join(",");
    headers.remove(name);
    if !kept.is_empty() {
        insert_header(headers, name, &kept);
    }
}

fn ensure_comma_header_value(headers: &mut HeaderMap, name: &'static str, required: &str) {
    let existing = header_str(headers, name);
    if existing
        .split(',')
        .map(str::trim)
        .any(|value| value == required)
    {
        return;
    }
    let value = if existing.is_empty() {
        required.into()
    } else {
        format!("{existing},{required}")
    };
    insert_header(headers, name, &value);
}

fn header_str<'a>(headers: &'a HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .trim()
}

fn is_hop_by_hop(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
    )
}

fn is_websocket_handshake_header(name: &str) -> bool {
    matches!(name.to_ascii_lowercase().as_str(), "connection" | "upgrade")
        || name.to_ascii_lowercase().starts_with("sec-websocket-")
}

fn copy_response_headers(target: &mut HeaderMap, source: &HeaderMap) {
    for (name, value) in source {
        if !is_hop_by_hop(name.as_str()) && *name != http::header::CONTENT_LENGTH {
            target.append(name, value.clone());
        }
    }
}

fn json_response(status: StatusCode, value: &impl Serialize) -> Response {
    let body = serde_json::to_vec_pretty(value).unwrap_or_else(|_| b"null".to_vec());
    let mut response = Response::builder()
        .status(status)
        .header(http::header::CONTENT_TYPE, "application/json")
        .body(Body::from(body))
        .expect("valid JSON response");
    response.headers_mut().insert(
        "x-content-type-options",
        HeaderValue::from_static("nosniff"),
    );
    response
}

fn text_response(status: StatusCode, value: &str) -> Response {
    Response::builder()
        .status(status)
        .header(http::header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(Body::from(value.to_owned()))
        .expect("valid text response")
}

fn text_headers() -> HeaderMap {
    HeaderMap::from_iter([(
        http::header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain; charset=utf-8"),
    )])
}

fn buffered_into_response(response: &BufferedResponse) -> Response {
    let mut builder = Response::builder().status(response.status);
    copy_response_headers(
        builder.headers_mut().expect("response builder headers"),
        &response.headers,
    );
    builder
        .body(Body::from(response.body.clone()))
        .expect("valid buffered response")
}

fn html_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&#39;")
}

fn urlencoding_component(value: &str) -> String {
    percent_encoding::utf8_percent_encode(value, percent_encoding::NON_ALPHANUMERIC).to_string()
}

fn outbound_client() -> anyhow::Result<Client> {
    Ok(Client::builder()
        .http1_only()
        .connect_timeout(Duration::from_secs(30))
        .pool_idle_timeout(Duration::from_secs(10))
        .pool_max_idle_per_host(256)
        .redirect(reqwest::redirect::Policy::none())
        .build()?)
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use axum::extract::connect_info::ConnectInfo;
    use axum::routing::any;
    use tempfile::TempDir;
    use tokio::net::TcpListener;

    use super::*;

    fn test_server(temp: &TempDir) -> Arc<Server> {
        let sessions = Arc::new(session::Store::new(temp.path().join("sessions.json")).unwrap());
        let scheduler = Arc::new(SchedulerRef::new(Scheduler::default()));
        Arc::new(Server::new(sessions, scheduler).unwrap())
    }

    fn request(method: Method, path: &str) -> Request<Body> {
        Request::builder()
            .method(method)
            .uri(path)
            .body(Body::empty())
            .unwrap()
    }

    #[test]
    fn upstream_paths_match_account_modes_and_provider_prefixes() {
        let upstreams = Upstreams::default();
        let mut account = Account {
            provider: Provider::Codex,
            auth_mode: AuthMode::Oauth,
            ..Account::default()
        };
        let uri: Uri = "/v1/responses?x=1".parse().unwrap();
        assert_eq!(
            target_url(&upstreams, &uri, &account).unwrap().as_str(),
            "https://chatgpt.com/backend-api/codex/responses?x=1"
        );
        account.auth_mode = AuthMode::ApiKey;
        assert_eq!(
            target_url(&upstreams, &uri, &account).unwrap().as_str(),
            "https://api.openai.com/v1/responses?x=1"
        );
        account.provider = Provider::Kimi;
        let uri: Uri = "/kimi/v1/messages".parse().unwrap();
        assert_eq!(
            target_url(&upstreams, &uri, &account).unwrap().as_str(),
            "https://api.kimi.com/coding/v1/messages"
        );
        account.provider = Provider::Zai;
        let uri: Uri = "/zai/chat/completions".parse().unwrap();
        assert_eq!(
            target_url(&upstreams, &uri, &account).unwrap().as_str(),
            "https://api.z.ai/api/coding/paas/v4/chat/completions"
        );
    }

    #[test]
    fn credentials_are_replaced_and_internal_headers_are_removed() {
        let mut source = HeaderMap::new();
        source.insert(
            http::header::AUTHORIZATION,
            HeaderValue::from_static("Bearer client"),
        );
        source.insert("x-subrouter-session", HeaderValue::from_static("private"));
        let account = Account {
            provider: Provider::Claude,
            auth_mode: AuthMode::Oauth,
            token: "provider-token".into(),
            ..Account::default()
        };
        let headers = authenticated_headers(&source, &account);
        assert_eq!(
            header_str(&headers, "authorization"),
            "Bearer provider-token"
        );
        assert_eq!(
            header_str(&headers, "anthropic-beta"),
            CLAUDE_OAUTH_BETA_HEADER
        );
        assert!(!headers.contains_key("x-subrouter-session"));
    }

    #[test]
    fn usage_limit_detection_handles_nested_envelopes() {
        assert!(usage_limit_json(
            br#"{"error":{"type":"usage_limit_reached"}}"#
        ));
        assert!(usage_limit_json(
            br#"{"message":"Usage limit has been exceeded"}"#
        ));
        assert!(!usage_limit_json(
            br#"{"error":{"type":"rate_limit_error"}}"#
        ));
    }

    #[tokio::test]
    async fn request_bodies_within_the_limit_are_replayable() {
        let body = prepare_request_body(Body::from("replay me"), 9)
            .await
            .unwrap();
        assert!(body.is_buffered());
        assert_eq!(body.buffered().unwrap(), "replay me");
        assert_eq!(body.probe(), "replay me");
    }

    #[tokio::test]
    async fn request_bodies_over_the_limit_stream_once_without_losing_prefix() {
        let incoming = futures_util::stream::iter([
            Ok::<_, std::convert::Infallible>(Bytes::from_static(b"abcd")),
            Ok(Bytes::from_static(b"efgh")),
        ]);
        let body = prepare_request_body(Body::from_stream(incoming), 5)
            .await
            .unwrap();
        assert!(!body.is_buffered());
        assert_eq!(body.probe(), "abcde");
        let mut stream = body.take_stream().unwrap();
        let mut received = Vec::new();
        while let Some(chunk) = stream.next().await {
            received.extend_from_slice(&chunk.unwrap());
        }
        assert_eq!(received, b"abcdefgh");
        assert!(body.take_stream().is_err());
    }

    #[tokio::test]
    async fn lifecycle_endpoints_transition_to_draining() {
        let temp = TempDir::new().unwrap();
        let server = test_server(&temp);

        let ready = Arc::clone(&server)
            .handle(request(Method::GET, "/_subrouter/ready"))
            .await;
        assert_eq!(ready.status(), StatusCode::OK);

        let drained = Arc::clone(&server)
            .handle(request(Method::POST, "/_subrouter/drain"))
            .await;
        assert_eq!(drained.status(), StatusCode::OK);

        let ready = Arc::clone(&server)
            .handle(request(Method::GET, "/_subrouter/ready"))
            .await;
        assert_eq!(ready.status(), StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test]
    async fn remote_control_endpoints_require_the_admin_token() {
        let temp = TempDir::new().unwrap();
        let mut server = Arc::try_unwrap(test_server(&temp)).ok().unwrap();
        server.admin_token = "server-secret".into();
        let server = Arc::new(server);

        let mut unauthorized = request(Method::GET, "/_subrouter/accounts");
        unauthorized
            .extensions_mut()
            .insert(ConnectInfo(ClientAddr(Some(
                "100.64.0.2:12345".parse().unwrap(),
            ))));
        let response = Arc::clone(&server).handle(unauthorized).await;
        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

        let mut authorized = request(Method::GET, "/_subrouter/accounts");
        authorized
            .extensions_mut()
            .insert(ConnectInfo(ClientAddr(Some(
                "100.64.0.2:12345".parse().unwrap(),
            ))));
        authorized.headers_mut().insert(
            http::header::AUTHORIZATION,
            HeaderValue::from_static("Bearer server-secret"),
        );
        let response = Arc::clone(&server).handle(authorized).await;
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn proxy_replaces_credentials_retries_and_preserves_the_body() {
        let attempts = Arc::new(AtomicUsize::new(0));
        let observed = Arc::new(Mutex::new(Vec::new()));
        let upstream_attempts = Arc::clone(&attempts);
        let upstream_observed = Arc::clone(&observed);
        let app = Router::new().fallback(any(move |request: Request<Body>| {
            let upstream_attempts = Arc::clone(&upstream_attempts);
            let upstream_observed = Arc::clone(&upstream_observed);
            async move {
                let authorization = request
                    .headers()
                    .get(http::header::AUTHORIZATION)
                    .and_then(|value| value.to_str().ok())
                    .unwrap_or_default()
                    .to_owned();
                let internal_header = request.headers().contains_key("x-subrouter-session");
                let body = to_bytes(request.into_body(), 1024).await.unwrap();
                upstream_observed
                    .lock()
                    .unwrap()
                    .push((authorization, internal_header, body));
                if upstream_attempts.fetch_add(1, Ordering::SeqCst) == 0 {
                    StatusCode::REQUEST_TIMEOUT.into_response()
                } else {
                    (StatusCode::OK, "proxied").into_response()
                }
            }
        }));
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let upstream = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let temp = TempDir::new().unwrap();
        let mut server = Arc::try_unwrap(test_server(&temp)).ok().unwrap();
        server.upstreams.override_upstream =
            Some(Url::parse(&format!("http://{address}")).unwrap());
        server.static_accounts.push(Account {
            id: "account@example.com".into(),
            provider: Provider::Codex,
            auth_mode: AuthMode::Oauth,
            token: "upstream-token".into(),
            ..Account::default()
        });
        let server = Arc::new(server);
        let mut proxied = Request::builder()
            .method(Method::POST)
            .uri("/v1/responses")
            .header(http::header::AUTHORIZATION, "Bearer client-token")
            .header("x-subrouter-session", "session-1")
            .body(Body::from("request-body"))
            .unwrap();
        proxied.extensions_mut().insert(ConnectInfo(ClientAddr(Some(
            "127.0.0.1:12345".parse().unwrap(),
        ))));

        let response = Arc::clone(&server).handle(proxied).await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            to_bytes(response.into_body(), 1024).await.unwrap(),
            "proxied"
        );
        assert_eq!(attempts.load(Ordering::SeqCst), 2);
        let observed = observed.lock().unwrap();
        assert_eq!(observed.len(), 2);
        for (authorization, internal_header, body) in observed.iter() {
            assert_eq!(authorization, "Bearer upstream-token");
            assert!(!internal_header);
            assert_eq!(body, "request-body");
        }
        upstream.abort();
    }
}
