use std::collections::HashMap;
use std::fs;
use std::io;
use std::net::IpAddr;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{anyhow, bail};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};
use futures_util::StreamExt;
use reqwest::{Client, Method, StatusCode, redirect::Policy};
use serde::de::Error as _;
use serde::{Deserialize, Deserializer, Serialize};
use serde_json::{Map, Value, json};
use url::Url;

use crate::account::{Account, AuthMode, Provider};

pub const DEFAULT_BASE_URL: &str = "https://cmux.com";

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum CredentialSource {
    Team,
    Local,
    Legacy,
    #[default]
    #[serde(rename = "")]
    Unspecified,
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Config {
    pub version: i32,
    pub base_url: String,
    pub access_token: String,
    pub refresh_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub local_proxy_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub team_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub team_name: String,
    #[serde(default, skip_serializing_if = "is_unspecified")]
    pub credential_source: CredentialSource,
}

const fn is_unspecified(source: &CredentialSource) -> bool {
    matches!(source, CredentialSource::Unspecified)
}

impl Config {
    #[must_use]
    pub fn logged_in(&self) -> bool {
        !self.access_token.trim().is_empty() && !self.refresh_token.trim().is_empty()
    }

    #[must_use]
    pub fn ready(&self) -> bool {
        self.logged_in() && !self.team_id.trim().is_empty()
    }

    #[must_use]
    pub fn effective_credential_source(&self) -> CredentialSource {
        match self.credential_source {
            CredentialSource::Unspecified if self.ready() => CredentialSource::Team,
            CredentialSource::Unspecified => CredentialSource::Legacy,
            source => source,
        }
    }

    #[must_use]
    pub fn team_mode_ready(&self) -> bool {
        self.effective_credential_source() == CredentialSource::Team && self.ready()
    }

    #[must_use]
    pub fn uses_local_credentials(&self) -> bool {
        self.effective_credential_source() == CredentialSource::Local
    }

    #[must_use]
    pub fn uses_legacy_server(&self) -> bool {
        self.effective_credential_source() == CredentialSource::Legacy
    }

    #[must_use]
    pub fn normalized(&self) -> Self {
        Self {
            version: if self.version == 0 { 1 } else { self.version },
            base_url: {
                let value = self.base_url.trim().trim_end_matches('/');
                if value.is_empty() {
                    DEFAULT_BASE_URL.into()
                } else {
                    value.into()
                }
            },
            access_token: self.access_token.trim().into(),
            refresh_token: self.refresh_token.trim().into(),
            local_proxy_token: self.local_proxy_token.trim().into(),
            team_id: self.team_id.trim().into(),
            team_name: self.team_name.trim().into(),
            credential_source: self.credential_source,
        }
    }

    pub fn validate(&self) -> anyhow::Result<()> {
        let normalized = self.normalized();
        let url = Url::parse(&normalized.base_url)
            .map_err(|error| anyhow!("invalid cmux.com base URL: {error}"))?;
        if url.host_str().is_none()
            || !url.username().is_empty()
            || url.password().is_some()
            || !matches!(url.path(), "" | "/")
            || url.query().is_some()
            || url.fragment().is_some()
        {
            bail!(
                "cmux.com base URL must be an origin without credentials, path, query, or fragment"
            );
        }
        let host = url.host_str().unwrap_or_default().to_ascii_lowercase();
        let loopback = host == "localhost"
            || IpAddr::from_str(&host).is_ok_and(|address| address.is_loopback());
        if url.scheme() != "https" && !(url.scheme() == "http" && loopback) {
            bail!("cmux.com base URL must use HTTPS, except for a loopback development server");
        }
        Ok(())
    }
}

pub fn default_config_path() -> anyhow::Result<PathBuf> {
    if let Some(path) = std::env::var_os("SUBROUTER_CLOUD_CONFIG")
        .filter(|value| !value.to_string_lossy().trim().is_empty())
    {
        return Ok(path.into());
    }
    let home = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .ok_or_else(|| anyhow!("home directory is empty"))?;
    Ok(PathBuf::from(home).join(".config/subrouter/cloud.json"))
}

pub fn load_config(path: Option<&Path>) -> anyhow::Result<Config> {
    let path = path
        .map(Path::to_path_buf)
        .map_or_else(default_config_path, Ok)?;
    let body = match fs::read(path) {
        Ok(body) => body,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(Config {
                version: 1,
                base_url: DEFAULT_BASE_URL.into(),
                ..Config::default()
            });
        }
        Err(error) => return Err(error.into()),
    };
    let config: Config = serde_json::from_slice(&body)?;
    let config = config.normalized();
    config.validate()?;
    Ok(config)
}

pub fn save_config(path: Option<&Path>, config: &Config) -> anyhow::Result<Config> {
    let path = path
        .map(Path::to_path_buf)
        .map_or_else(default_config_path, Ok)?;
    let mut config = config.normalized();
    if config.local_proxy_token.is_empty() {
        config.local_proxy_token = URL_SAFE_NO_PAD.encode(rand::random::<[u8; 32]>());
    }
    config.validate()?;
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let mut body = serde_json::to_vec_pretty(&config)?;
    body.push(b'\n');
    let mut temp = tempfile::Builder::new()
        .prefix(".cloud-")
        .suffix(".tmp")
        .tempfile_in(parent)?;
    use std::io::Write as _;
    temp.write_all(&body)?;
    temp.as_file().sync_all()?;
    set_private_permissions(temp.path())?;
    temp.persist(path).map_err(|error| error.error)?;
    Ok(config)
}

pub fn delete_config(path: Option<&Path>) -> anyhow::Result<()> {
    let path = path
        .map(Path::to_path_buf)
        .map_or_else(default_config_path, Ok)?;
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

#[cfg(unix)]
fn set_private_permissions(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))
}

#[cfg(not(unix))]
fn set_private_permissions(_path: &Path) -> io::Result<()> {
    Ok(())
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AuthStart {
    pub device_code: String,
    pub user_code: String,
    pub verification_url: String,
    pub expires_in_seconds: i32,
    pub interval_seconds: i32,
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AuthPoll {
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub refresh_token: String,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Team {
    pub id: String,
    pub name: String,
    pub personal: bool,
    pub use_permission: bool,
    pub manage_accounts: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SharedAccount {
    pub id: String,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub label: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub health: Option<SharedAccountHealth>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct SharedAccountHealth {
    pub ok: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
}

pub type AccountUpload = Map<String, Value>;

#[derive(Clone, Debug)]
pub struct LeaseRequest {
    pub provider: Provider,
    pub required_auth_mode: Option<AuthMode>,
    pub agent_type: String,
    pub session_id: String,
    pub user_email: String,
    pub prefer_account_id: String,
    pub model: String,
}

#[derive(Clone)]
pub struct Lease {
    pub id: String,
    pub account: Account,
    pub credential_generation: i32,
    pub issued_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub credential_expires_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum LeaseOutcome {
    Success,
    Unauthorized,
    Forbidden,
    RateLimited,
    ProviderError,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum LeaseCooldownScope {
    Account,
    Quota,
}

#[derive(Clone, Debug)]
pub struct LeaseReport {
    pub outcome: LeaseOutcome,
    pub status_code: Option<u16>,
    pub cooldown_scope: Option<LeaseCooldownScope>,
    pub retry_at: Option<DateTime<Utc>>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct LeaseWirePlain {
    lease_id: String,
    account_id: String,
    provider: String,
    auth_mode: String,
    token: String,
    #[serde(default)]
    provider_account_id: String,
    label: String,
    #[serde(default)]
    email: String,
    credential_generation: i32,
    issued_at: String,
    expires_at: String,
    #[serde(default)]
    credential_expires_at: String,
}

struct LeaseWire(LeaseWirePlain);

impl<'de> Deserialize<'de> for LeaseWire {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let value = Value::deserialize(deserializer)?;
        let object = value
            .as_object()
            .ok_or_else(|| D::Error::custom("credential lease must be an object"))?;
        for forbidden in [
            "refreshToken",
            "refresh_token",
            "idToken",
            "id_token",
            "credentials",
            "tokens",
            "apiKey",
        ] {
            if object.contains_key(forbidden) {
                return Err(D::Error::custom(format!(
                    "cmux.com credential lease contained forbidden field {forbidden:?}"
                )));
            }
        }
        serde_json::from_value(value)
            .map(Self)
            .map_err(D::Error::custom)
    }
}

#[derive(Default)]
struct Cache {
    by_key: HashMap<String, Lease>,
    lease_to_key: HashMap<String, String>,
    lease_refs: HashMap<String, LeaseRef>,
}

struct LeaseRef {
    account_id: String,
    credential_generation: i32,
    expires_at: DateTime<Utc>,
}

#[derive(Clone)]
pub struct BrokerClient {
    config: Config,
    http: Client,
    cache: Arc<Mutex<Cache>>,
}

impl BrokerClient {
    pub fn new(config: Config) -> anyhow::Result<Self> {
        let http = Client::builder()
            .timeout(Duration::from_secs(30))
            .redirect(Policy::none())
            .build()?;
        Ok(Self {
            config: config.normalized(),
            http,
            cache: Arc::new(Mutex::new(Cache::default())),
        })
    }

    #[must_use]
    pub fn with_client(config: Config, http: Client) -> Self {
        Self {
            config: config.normalized(),
            http,
            cache: Arc::new(Mutex::new(Cache::default())),
        }
    }

    pub async fn start_auth(&self) -> anyhow::Result<AuthStart> {
        self.request(
            Method::POST,
            "/api/vault/cli/auth/start",
            Some(json!({"client": "subrouter"})),
            false,
        )
        .await
    }

    pub async fn poll_auth(&self, device_code: &str) -> anyhow::Result<AuthPoll> {
        self.request(
            Method::POST,
            "/api/vault/cli/auth/poll",
            Some(json!({"deviceCode": device_code})),
            false,
        )
        .await
    }

    pub async fn logout(&self) -> anyhow::Result<()> {
        self.request::<Value>(Method::POST, "/api/subrouter/logout", None, true)
            .await
            .map(|_| ())
    }

    pub async fn list_teams(&self) -> anyhow::Result<(Vec<Team>, String)> {
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase")]
        struct Envelope {
            selected_team_id: String,
            teams: Vec<TeamWire>,
        }
        #[derive(Deserialize)]
        struct TeamWire {
            id: String,
            name: String,
            personal: bool,
            permissions: Permissions,
        }
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase")]
        struct Permissions {
            #[serde(rename = "use")]
            use_permission: bool,
            manage_accounts: bool,
        }
        let response: Envelope = self
            .request(Method::GET, "/api/subrouter/teams", None, true)
            .await?;
        Ok((
            response
                .teams
                .into_iter()
                .map(|team| Team {
                    id: team.id,
                    name: team.name,
                    personal: team.personal,
                    use_permission: team.permissions.use_permission,
                    manage_accounts: team.permissions.manage_accounts,
                })
                .collect(),
            response.selected_team_id,
        ))
    }

    pub async fn list_accounts(&self) -> anyhow::Result<Vec<SharedAccount>> {
        #[derive(Deserialize)]
        struct Envelope {
            accounts: Vec<SharedAccount>,
        }
        Ok(self
            .request::<Envelope>(Method::GET, "/api/subrouter/accounts", None, true)
            .await?
            .accounts)
    }

    pub async fn upload_account(&self, upload: AccountUpload) -> anyhow::Result<SharedAccount> {
        self.account_mutation(Method::POST, "/api/subrouter/accounts?adopt=1", upload)
            .await
    }

    pub async fn delete_account(&self, account_id: &str) -> anyhow::Result<()> {
        self.request::<Value>(
            Method::DELETE,
            &format!("/api/subrouter/accounts/{}", encode_path(account_id)),
            None,
            true,
        )
        .await
        .map(|_| ())
    }

    pub async fn repair_account(
        &self,
        account_id: &str,
        upload: AccountUpload,
    ) -> anyhow::Result<SharedAccount> {
        self.account_mutation(
            Method::POST,
            &format!(
                "/api/subrouter/accounts/{}/repair?adopt=1",
                encode_path(account_id)
            ),
            upload,
        )
        .await
    }

    async fn account_mutation(
        &self,
        method: Method,
        path: &str,
        upload: AccountUpload,
    ) -> anyhow::Result<SharedAccount> {
        #[derive(Deserialize)]
        struct Envelope {
            account: SharedAccount,
        }
        Ok(self
            .request::<Envelope>(method, path, Some(Value::Object(upload)), true)
            .await?
            .account)
    }

    pub async fn lease(&self, input: &LeaseRequest) -> anyhow::Result<Lease> {
        let key = lease_cache_key(input);
        let now = Utc::now();
        {
            let mut cache = self
                .cache
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            cache
                .lease_refs
                .retain(|_, reference| reference.expires_at > now);
            let live_ids: std::collections::HashSet<_> = cache.lease_refs.keys().cloned().collect();
            cache
                .lease_to_key
                .retain(|lease_id, _| live_ids.contains(lease_id));
            cache
                .by_key
                .retain(|_, lease| lease.expires_at > now + chrono::Duration::seconds(15));
            if let Some(lease) = cache.by_key.get(&key) {
                return Ok(lease.clone());
            }
        }
        let mut body = Map::from_iter([
            ("provider".into(), input.provider.as_str().into()),
            ("agentType".into(), input.agent_type.clone().into()),
            ("sessionId".into(), input.session_id.clone().into()),
        ]);
        for (name, value) in [
            ("userEmail", input.user_email.as_str()),
            ("preferAccountId", input.prefer_account_id.as_str()),
            ("model", input.model.as_str()),
        ] {
            if !value.is_empty() {
                body.insert(name.into(), value.into());
            }
        }
        if let Some(auth_mode) = input.required_auth_mode {
            body.insert(
                "requiredAuthMode".into(),
                match auth_mode {
                    AuthMode::Oauth => "oauth",
                    AuthMode::ApiKey => "apikey",
                }
                .into(),
            );
        }
        #[derive(Deserialize)]
        struct Envelope {
            lease: LeaseWire,
        }
        let raw = self
            .request::<Envelope>(
                Method::POST,
                "/api/subrouter/leases",
                Some(Value::Object(body)),
                true,
            )
            .await?
            .lease
            .0;
        let lease = parse_lease(raw)?;
        if input
            .required_auth_mode
            .is_some_and(|mode| lease.account.auth_mode != mode)
        {
            bail!("cmux.com returned a credential with the wrong auth mode");
        }
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        cache.by_key.insert(key.clone(), lease.clone());
        cache.lease_to_key.insert(lease.id.clone(), key);
        cache.lease_refs.insert(
            lease.id.clone(),
            LeaseRef {
                account_id: lease.account.id.clone(),
                credential_generation: lease.credential_generation,
                expires_at: lease.expires_at,
            },
        );
        Ok(lease)
    }

    pub async fn report(&self, lease_id: &str, report: &LeaseReport) -> anyhow::Result<()> {
        let mut body = Map::from_iter([("outcome".into(), serde_json::to_value(report.outcome)?)]);
        if let Some(status) = report.status_code {
            body.insert("statusCode".into(), status.into());
        }
        if let Some(scope) = report.cooldown_scope {
            body.insert("scope".into(), serde_json::to_value(scope)?);
        }
        if let Some(retry_at) = report.retry_at {
            body.insert("retryAt".into(), retry_at.timestamp_millis().into());
        }
        let result = self
            .request::<Value>(
                Method::POST,
                &format!("/api/subrouter/leases/{}/events", encode_path(lease_id)),
                Some(Value::Object(body)),
                true,
            )
            .await
            .map(|_| ());
        if matches!(
            report.outcome,
            LeaseOutcome::Unauthorized | LeaseOutcome::Forbidden | LeaseOutcome::RateLimited
        ) {
            self.invalidate_lease(lease_id);
        }
        result
    }

    pub fn invalidate_lease(&self, lease_id: &str) {
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let Some(reference) = cache.lease_refs.get(lease_id) else {
            if let Some(key) = cache.lease_to_key.remove(lease_id) {
                cache.by_key.remove(&key);
            }
            return;
        };
        let account_id = reference.account_id.clone();
        let generation = reference.credential_generation;
        cache.by_key.retain(|_, lease| {
            lease.account.id != account_id || lease.credential_generation != generation
        });
        let invalid_ids: Vec<_> = cache
            .lease_refs
            .iter()
            .filter(|(_, reference)| {
                reference.account_id == account_id && reference.credential_generation == generation
            })
            .map(|(id, _)| id.clone())
            .collect();
        for id in invalid_ids {
            cache.lease_refs.remove(&id);
            cache.lease_to_key.remove(&id);
        }
    }

    async fn request<T: for<'de> Deserialize<'de>>(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
        auth: bool,
    ) -> anyhow::Result<T> {
        self.config.validate()?;
        let mut request = self
            .http
            .request(method, format!("{}{path}", self.config.base_url))
            .header(reqwest::header::ACCEPT, "application/json");
        if let Some(body) = body {
            request = request.json(&body);
        }
        if auth {
            if !self.config.logged_in() {
                bail!("not logged in; run 'sr login'");
            }
            request = request
                .bearer_auth(&self.config.access_token)
                .header("X-Stack-Refresh-Token", &self.config.refresh_token);
            if !self.config.team_id.is_empty() {
                request = request.header("X-Cmux-Team-ID", &self.config.team_id);
            }
        }
        let response = request.send().await?;
        let status = response.status();
        let body = read_limited(response, 2 << 20).await?;
        if !status.is_success() {
            return Err(api_error(status));
        }
        if body.is_empty() {
            return serde_json::from_value(Value::Null).map_err(Into::into);
        }
        serde_json::from_slice(&body).map_err(|_| anyhow!("cmux.com returned an invalid response"))
    }
}

async fn read_limited(response: reqwest::Response, limit: usize) -> anyhow::Result<Vec<u8>> {
    let mut body = Vec::new();
    let mut stream = response.bytes_stream();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk?;
        if body.len().saturating_add(chunk.len()) > limit {
            bail!("cmux.com response exceeded {limit} bytes");
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

fn parse_lease(raw: LeaseWirePlain) -> anyhow::Result<Lease> {
    if raw.lease_id.is_empty() || raw.account_id.is_empty() || raw.token.is_empty() {
        bail!("cmux.com returned an incomplete credential lease");
    }
    let provider: Provider = raw
        .provider
        .parse()
        .map_err(|_| anyhow!("cmux.com returned an invalid lease provider"))?;
    if !matches!(provider, Provider::Codex | Provider::Claude) {
        bail!("cmux.com returned an invalid lease provider");
    }
    let auth_mode = match raw.auth_mode.as_str() {
        "oauth" => AuthMode::Oauth,
        "apikey" => AuthMode::ApiKey,
        _ => bail!("cmux.com returned an invalid lease auth mode"),
    };
    let issued_at = DateTime::parse_from_rfc3339(&raw.issued_at)
        .map_err(|_| anyhow!("cmux.com returned an invalid lease issue time"))?
        .with_timezone(&Utc);
    let expires_at = DateTime::parse_from_rfc3339(&raw.expires_at)
        .map_err(|_| anyhow!("cmux.com returned an invalid lease expiry"))?
        .with_timezone(&Utc);
    if expires_at <= Utc::now() {
        bail!("cmux.com returned an expired credential lease");
    }
    let credential_expires_at = if raw.credential_expires_at.is_empty() {
        None
    } else {
        Some(
            DateTime::parse_from_rfc3339(&raw.credential_expires_at)
                .map_err(|_| anyhow!("cmux.com returned an invalid credential expiry"))?
                .with_timezone(&Utc),
        )
    };
    Ok(Lease {
        id: raw.lease_id,
        account: Account {
            id: raw.account_id,
            provider,
            auth_mode,
            label: raw.label,
            email: raw.email,
            token: raw.token,
            account_id: raw.provider_account_id,
            source: "cmux-team-vault".into(),
            ..Account::default()
        },
        credential_generation: raw.credential_generation,
        issued_at,
        expires_at,
        credential_expires_at,
    })
}

fn lease_cache_key(input: &LeaseRequest) -> String {
    [
        input.provider.as_str(),
        input.required_auth_mode.map_or("", |mode| match mode {
            AuthMode::Oauth => "oauth",
            AuthMode::ApiKey => "apikey",
        }),
        &input.agent_type,
        &input.session_id,
        &input.user_email,
        &input.prefer_account_id,
        &input.model,
    ]
    .join("\0")
}

fn encode_path(value: &str) -> String {
    percent_encoding::utf8_percent_encode(value, percent_encoding::NON_ALPHANUMERIC).to_string()
}

fn api_error(status: StatusCode) -> anyhow::Error {
    anyhow!(
        "cmux.com request failed ({}): {}",
        status.as_u16(),
        status.canonical_reason().unwrap_or("request failed")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_normalizes_and_restricts_origins() {
        let config = Config {
            base_url: " https://cmux.com/ ".into(),
            ..Config::default()
        }
        .normalized();
        assert_eq!(config.base_url, "https://cmux.com");
        config.validate().unwrap();
        for invalid in [
            "http://cmux.com",
            "https://user@cmux.com",
            "https://cmux.com/api",
            "https://cmux.com?q=1",
        ] {
            assert!(
                Config {
                    base_url: invalid.into(),
                    ..Config::default()
                }
                .validate()
                .is_err(),
                "accepted {invalid}"
            );
        }
        Config {
            base_url: "http://127.0.0.1:3000".into(),
            ..Config::default()
        }
        .validate()
        .unwrap();
    }

    #[test]
    fn config_save_generates_private_local_token() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("cloud.json");
        let saved = save_config(Some(&path), &Config::default()).unwrap();
        assert_eq!(saved.local_proxy_token.len(), 43);
        assert!(load_config(Some(&path)).unwrap() == saved);
    }

    #[test]
    fn lease_rejects_refresh_tokens_before_deserializing() {
        let body = json!({
            "leaseId": "lease",
            "accountId": "account",
            "provider": "codex",
            "authMode": "oauth",
            "token": "access",
            "label": "label",
            "credentialGeneration": 1,
            "issuedAt": Utc::now().to_rfc3339(),
            "expiresAt": (Utc::now() + chrono::Duration::minutes(1)).to_rfc3339(),
            "refreshToken": "must-not-cross-boundary"
        });
        let error = match serde_json::from_value::<LeaseWire>(body) {
            Ok(_) => panic!("lease with refresh token was accepted"),
            Err(error) => error.to_string(),
        };
        assert!(error.contains("forbidden field"));
        assert!(!error.contains("must-not-cross-boundary"));
    }
}
