use std::collections::HashMap;
use std::io::{Cursor, Read};
use std::sync::Mutex;
use std::time::Duration;

use anyhow::{anyhow, bail};
use base64::{Engine, engine::general_purpose};
use chrono::{DateTime, SecondsFormat, Utc};
use http::{HeaderMap, Method, Uri};
use serde::de::{IgnoredAny, MapAccess, Visitor};
use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::account::{Account, AuthMode, Provider};
use crate::session::normalize_model;

pub const DEFAULT_TTL: Duration = Duration::from_secs(15 * 60);
const ROTATION_GRACE: Duration = Duration::from_secs(30);
const RENEW_RETRY_TTL: Duration = Duration::from_secs(2 * 60);
const TOKEN_TYPE: &str = "SRLEASE";
const SYNTHETIC_CHATGPT_ACCOUNT_ID: &str = "cloudmux-broker";

#[derive(Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct LeaseRequest {
    pub organization_id: String,
    pub workspace_id: String,
    pub conversation_id: String,
    pub invocation_id: String,
    pub agent_session_id: String,
    pub agent: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub proxy_base_url: String,
}

impl LeaseRequest {
    pub fn normalize_and_validate(&mut self) -> anyhow::Result<(Provider, String)> {
        for value in [
            &mut self.organization_id,
            &mut self.workspace_id,
            &mut self.conversation_id,
            &mut self.invocation_id,
            &mut self.agent_session_id,
        ] {
            *value = value.trim().to_owned();
            if value.is_empty() || value.len() > 256 {
                bail!("session lease identifiers are required and must be at most 256 bytes");
            }
        }
        self.agent = self.agent.trim().to_ascii_lowercase();
        self.provider = self.provider.trim().to_ascii_lowercase();
        self.model = self.model.trim().to_owned();
        self.proxy_base_url = self.proxy_base_url.trim().trim_end_matches('/').to_owned();
        if self.agent != "pi" {
            bail!("agent must be pi");
        }
        if self.model.len() > 256 {
            bail!("model must be at most 256 bytes");
        }
        provider_and_model(&self.provider, &self.model)
    }

    #[must_use]
    pub fn scope_key(&self, provider: Provider, model: &str) -> String {
        let mut key = String::new();
        for component in [
            self.organization_id.as_str(),
            self.workspace_id.as_str(),
            self.conversation_id.as_str(),
            self.invocation_id.as_str(),
            self.agent_session_id.as_str(),
            provider.as_str(),
            model,
        ] {
            key.push_str(&format!("{}:{component}", component.len()));
        }
        key
    }

    #[must_use]
    pub fn routing_key(&self, provider: Provider, model: &str) -> String {
        let digest = Sha256::digest(self.scope_key(provider, model));
        format!(
            "lease-route-{}",
            general_purpose::URL_SAFE_NO_PAD.encode(&digest[..18])
        )
    }
}

#[derive(Clone)]
pub struct Lease {
    pub id: String,
    token: String,
    pub scope_key: String,
    pub organization_id: String,
    pub workspace_id: String,
    pub conversation_id: String,
    pub invocation_id: String,
    pub session_key: String,
    pub agent: String,
    pub provider: Provider,
    pub account_id: String,
    pub auth_mode: AuthMode,
    pub model: String,
    pub proxy_base_url: String,
    pub created_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
}

impl Lease {
    #[must_use]
    pub fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn allows_account(&self, account: &Account) -> bool {
        self.account_id == account.id
            && self.provider == account.provider
            && self.auth_mode == account.auth_mode
    }

    #[must_use]
    pub fn allows_endpoint(&self, method: &Method, path: &str) -> bool {
        if *method != Method::POST {
            return false;
        }
        match self.provider {
            Provider::Codex => matches!(
                path,
                "/responses"
                    | "/v1/responses"
                    | "/responses/compact"
                    | "/v1/responses/compact"
                    | "/backend-api/codex/responses"
                    | "/backend-api/codex/responses/compact"
            ),
            Provider::Claude => path == "/v1/messages",
            Provider::Kimi => path == "/kimi/v1/messages",
            Provider::Zai => path == "/zai/chat/completions",
        }
    }

    pub fn validate_model(
        &self,
        uri: &Uri,
        headers: &HeaderMap,
        wire_body: &[u8],
    ) -> anyhow::Result<()> {
        if self.model.is_empty() {
            return Ok(());
        }
        for (key, value) in url::form_urlencoded::parse(uri.query().unwrap_or_default().as_bytes())
        {
            if key == "model" && normalize_model(&value).as_deref() != Some(self.model.as_str()) {
                bail!("request query model conflicts with lease");
            }
        }
        let content_type = headers
            .get(http::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default();
        if !content_type.to_ascii_lowercase().contains("json") {
            bail!("JSON request body is required");
        }
        let body = decode_body(wire_body, headers)?;
        let models = all_top_level_models(&body)?;
        if models.is_empty() {
            bail!("request body model is required");
        }
        if models.iter().any(|model| model != &self.model) {
            bail!("request body model conflicts with lease");
        }
        Ok(())
    }

    #[must_use]
    pub fn response(&self) -> LeaseResponse {
        let root = self.proxy_base_url.trim_end_matches('/');
        let (api, base_url, pi_base_url) = match self.provider {
            Provider::Codex => (
                "openai-codex-responses",
                format!("{root}/backend-api/codex"),
                format!("{root}/backend-api"),
            ),
            Provider::Claude => ("anthropic-messages", root.to_owned(), root.to_owned()),
            Provider::Kimi => (
                "anthropic-messages",
                format!("{root}/kimi"),
                format!("{root}/kimi"),
            ),
            Provider::Zai => (
                "openai-completions",
                format!("{root}/zai"),
                format!("{root}/zai"),
            ),
        };
        let mut environment =
            HashMap::from([("CLOUDMUX_SUBROUTER_LEASE_TOKEN".into(), self.token.clone())]);
        match self.provider {
            Provider::Codex | Provider::Zai => {
                environment.insert("OPENAI_API_KEY".into(), self.token.clone());
                environment.insert("OPENAI_BASE_URL".into(), base_url);
            }
            Provider::Claude | Provider::Kimi => {
                environment.insert("ANTHROPIC_API_KEY".into(), self.token.clone());
                environment.insert("ANTHROPIC_AUTH_TOKEN".into(), self.token.clone());
                environment.insert("ANTHROPIC_BASE_URL".into(), base_url);
            }
        }
        LeaseResponse {
            lease_id: self.id.clone(),
            session_key: self.session_key.clone(),
            expires_at: self.expires_at.to_rfc3339_opts(SecondsFormat::Nanos, true),
            environment,
            assignment: LeaseAssignment {
                account_id: self.account_id.clone(),
                provider: self.provider.to_string(),
                auth_mode: match self.auth_mode {
                    AuthMode::Oauth => "oauth",
                    AuthMode::ApiKey => "apikey",
                }
                .into(),
                model: self.model.clone(),
                reason: "subrouter_scheduler".into(),
            },
            pi: LeasePiConfig {
                provider: "cloudmux-subrouter".into(),
                api: api.into(),
                base_url: pi_base_url,
                api_key_environment_variable: "CLOUDMUX_SUBROUTER_LEASE_TOKEN".into(),
                model: self.model.clone(),
            },
        }
    }
}

#[derive(Clone)]
pub struct LeaseTemplate {
    pub scope_key: String,
    pub organization_id: String,
    pub workspace_id: String,
    pub conversation_id: String,
    pub invocation_id: String,
    pub session_key: String,
    pub agent: String,
    pub provider: Provider,
    pub account_id: String,
    pub auth_mode: AuthMode,
    pub model: String,
    pub proxy_base_url: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LeaseResponse {
    pub lease_id: String,
    pub session_key: String,
    pub expires_at: String,
    pub environment: HashMap<String, String>,
    pub assignment: LeaseAssignment,
    pub pi: LeasePiConfig,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LeaseAssignment {
    pub account_id: String,
    pub provider: String,
    pub auth_mode: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    pub reason: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LeasePiConfig {
    pub provider: String,
    pub api: String,
    pub base_url: String,
    pub api_key_environment_variable: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
}

#[derive(Clone, Debug)]
struct TokenBinding {
    lease_id: String,
    request_expires_at: DateTime<Utc>,
    renew_expires_at: DateTime<Utc>,
}

#[derive(Default)]
struct State {
    by_id: HashMap<String, Lease>,
    by_scope: HashMap<String, String>,
    by_token: HashMap<[u8; 32], TokenBinding>,
    tokens_by_id: HashMap<String, Vec<[u8; 32]>>,
}

pub struct LeaseStore {
    state: Mutex<State>,
    ttl: Duration,
}

impl Default for LeaseStore {
    fn default() -> Self {
        Self {
            state: Mutex::new(State::default()),
            ttl: DEFAULT_TTL,
        }
    }
}

#[derive(Debug, thiserror::Error, Eq, PartialEq)]
pub enum LeaseError {
    #[error("invalid or expired session lease")]
    Invalid,
    #[error("session lease not found")]
    NotFound,
    #[error("session lease token generation failed")]
    Generation,
}

impl LeaseStore {
    pub fn put(&self, template: LeaseTemplate) -> Result<Lease, LeaseError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = Utc::now();
        prune(&mut state, now);
        if let Some(lease) = state
            .by_scope
            .get(&template.scope_key)
            .and_then(|id| state.by_id.get(id))
            .cloned()
        {
            return Ok(lease);
        }
        let expires_at =
            now + chrono::Duration::from_std(self.ttl).map_err(|_| LeaseError::Generation)?;
        let id = format!(
            "lease_{}",
            general_purpose::URL_SAFE_NO_PAD.encode(rand::random::<[u8; 18]>())
        );
        let token = new_token(now, expires_at)?;
        let lease = Lease {
            id: id.clone(),
            token: token.clone(),
            scope_key: template.scope_key.clone(),
            organization_id: template.organization_id,
            workspace_id: template.workspace_id,
            conversation_id: template.conversation_id,
            invocation_id: template.invocation_id,
            session_key: template.session_key,
            agent: template.agent,
            provider: template.provider,
            account_id: template.account_id,
            auth_mode: template.auth_mode,
            model: template.model,
            proxy_base_url: template.proxy_base_url,
            created_at: now,
            expires_at,
        };
        state.by_scope.insert(template.scope_key, id.clone());
        state.by_id.insert(id.clone(), lease.clone());
        bind(&mut state, &id, &token, expires_at, expires_at);
        Ok(lease)
    }

    pub fn resolve(&self, token: &str) -> Result<Lease, LeaseError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = Utc::now();
        prune(&mut state, now);
        let binding = state
            .by_token
            .get(&token_hash(token))
            .filter(|binding| now < binding.request_expires_at)
            .ok_or(LeaseError::Invalid)?;
        state
            .by_id
            .get(&binding.lease_id)
            .cloned()
            .ok_or(LeaseError::Invalid)
    }

    pub fn renew(&self, id: &str, token: &str) -> Result<Lease, LeaseError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = Utc::now();
        prune(&mut state, now);
        let current = state.by_id.get(id).cloned().ok_or(LeaseError::NotFound)?;
        let hash = token_hash(token);
        let binding = state
            .by_token
            .get(&hash)
            .filter(|binding| binding.lease_id == id && now < binding.renew_expires_at)
            .cloned()
            .ok_or(LeaseError::Invalid)?;
        if hash != token_hash(&current.token) {
            return Ok(current);
        }
        let expires_at =
            now + chrono::Duration::from_std(self.ttl).map_err(|_| LeaseError::Generation)?;
        let next_token = new_token(now, expires_at)?;
        if let Some(old) = state.by_token.get_mut(&hash) {
            old.request_expires_at = old
                .request_expires_at
                .min(now + chrono::Duration::from_std(ROTATION_GRACE).unwrap());
            old.renew_expires_at = binding
                .renew_expires_at
                .min(now + chrono::Duration::from_std(RENEW_RETRY_TTL).unwrap());
        }
        let mut next = current;
        next.token.clone_from(&next_token);
        next.expires_at = expires_at;
        state.by_id.insert(id.into(), next.clone());
        bind(&mut state, id, &next_token, expires_at, expires_at);
        Ok(next)
    }

    pub fn release(&self, id: &str) -> bool {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let Some(lease) = state.by_id.remove(id) else {
            return false;
        };
        state.by_scope.remove(&lease.scope_key);
        if let Some(hashes) = state.tokens_by_id.remove(id) {
            for hash in hashes {
                state.by_token.remove(&hash);
            }
        }
        true
    }
}

fn bind(
    state: &mut State,
    id: &str,
    token: &str,
    request_expires_at: DateTime<Utc>,
    renew_expires_at: DateTime<Utc>,
) {
    let hash = token_hash(token);
    state.by_token.insert(
        hash,
        TokenBinding {
            lease_id: id.into(),
            request_expires_at,
            renew_expires_at,
        },
    );
    state.tokens_by_id.entry(id.into()).or_default().push(hash);
}

fn prune(state: &mut State, now: DateTime<Utc>) {
    let expired: Vec<String> = state
        .by_id
        .iter()
        .filter(|(_, lease)| now >= lease.expires_at)
        .map(|(id, _)| id.clone())
        .collect();
    for id in expired {
        if let Some(lease) = state.by_id.remove(&id) {
            state.by_scope.remove(&lease.scope_key);
        }
        if let Some(hashes) = state.tokens_by_id.remove(&id) {
            for hash in hashes {
                state.by_token.remove(&hash);
            }
        }
    }
}

fn token_hash(token: &str) -> [u8; 32] {
    Sha256::digest(token).into()
}

fn new_token(issued_at: DateTime<Utc>, expires_at: DateTime<Utc>) -> Result<String, LeaseError> {
    let header = serde_json::json!({"alg":"none","typ":TOKEN_TYPE});
    let payload = serde_json::json!({
        "iss":"subrouter",
        "aud":"cloudmux",
        "iat":issued_at.timestamp(),
        "exp":expires_at.timestamp(),
        "jti":Uuid::new_v4().to_string(),
        "cloudmux_session_lease":true,
        "https://api.openai.com/auth":{"chatgpt_account_id":SYNTHETIC_CHATGPT_ACCOUNT_ID}
    });
    let header = serde_json::to_vec(&header).map_err(|_| LeaseError::Generation)?;
    let payload = serde_json::to_vec(&payload).map_err(|_| LeaseError::Generation)?;
    let signature = general_purpose::URL_SAFE_NO_PAD.encode(rand::random::<[u8; 24]>());
    Ok(format!(
        "{}.{}.{}",
        general_purpose::STANDARD_NO_PAD.encode(header),
        general_purpose::STANDARD_NO_PAD.encode(payload),
        signature
    ))
}

pub fn presented_token(headers: &HeaderMap) -> Option<String> {
    if let Some(value) = headers
        .get("x-subrouter-lease")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        return Some(value.into());
    }
    let bearer = headers
        .get(http::header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split_once(' '))
        .filter(|(kind, _)| kind.eq_ignore_ascii_case("bearer"))
        .map(|(_, token)| token.trim());
    for value in [
        bearer,
        headers
            .get("x-api-key")
            .and_then(|value| value.to_str().ok())
            .map(str::trim),
    ]
    .into_iter()
    .flatten()
    {
        if looks_like_token(value) {
            return Some(value.into());
        }
    }
    None
}

#[must_use]
pub fn looks_like_token(value: &str) -> bool {
    if value.is_empty() || value.len() > 4096 {
        return false;
    }
    let mut parts = value.split('.');
    let (Some(header), Some(payload), Some(signature), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return false;
    };
    if signature.is_empty() {
        return false;
    }
    let Some(header) =
        decode_segment(header).and_then(|body| serde_json::from_slice::<Value>(&body).ok())
    else {
        return false;
    };
    if header.get("typ").and_then(Value::as_str) != Some(TOKEN_TYPE) {
        return false;
    }
    decode_segment(payload)
        .and_then(|body| serde_json::from_slice::<Value>(&body).ok())
        .and_then(|body| body.get("cloudmux_session_lease").and_then(Value::as_bool))
        == Some(true)
}

fn decode_segment(value: &str) -> Option<Vec<u8>> {
    [
        &general_purpose::STANDARD_NO_PAD,
        &general_purpose::STANDARD,
        &general_purpose::URL_SAFE_NO_PAD,
        &general_purpose::URL_SAFE,
    ]
    .into_iter()
    .find_map(|engine| engine.decode(value).ok())
}

fn provider_and_model(
    provider_value: &str,
    model_value: &str,
) -> anyhow::Result<(Provider, String)> {
    let mut provider_name = provider_value.trim().to_ascii_lowercase();
    let mut model = model_value.trim().to_owned();
    if provider_name.is_empty() {
        if let Some((prefix, rest)) = model.split_once('/') {
            provider_name = prefix.trim().to_ascii_lowercase();
            model = rest.trim().to_owned();
        } else {
            let lower = model.to_ascii_lowercase();
            provider_name = if lower.starts_with("claude-") {
                "claude"
            } else if lower.starts_with("kimi-") {
                "kimi"
            } else if lower.starts_with("glm-") {
                "zai"
            } else {
                "codex"
            }
            .into();
        }
    }
    let provider = parse_provider(&provider_name)?;
    if let Some((prefix, rest)) = model.clone().split_once('/')
        && let Ok(model_provider) = parse_provider(prefix.trim())
    {
        if model_provider != provider {
            bail!("provider and model provider do not match");
        }
        model = rest.trim().to_owned();
    }
    Ok((provider, model))
}

fn parse_provider(value: &str) -> anyhow::Result<Provider> {
    match value.trim().to_ascii_lowercase().as_str() {
        "codex" | "openai" | "openai-codex" => Ok(Provider::Codex),
        "claude" | "anthropic" => Ok(Provider::Claude),
        "kimi" | "kimi-for-coding" => Ok(Provider::Kimi),
        "zai" | "glm" => Ok(Provider::Zai),
        value => Err(anyhow!("unsupported provider {value:?}")),
    }
}

fn decode_body(wire_body: &[u8], headers: &HeaderMap) -> anyhow::Result<Vec<u8>> {
    let encoding = headers
        .get(http::header::CONTENT_ENCODING)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    match encoding.as_str() {
        "" | "identity" => Ok(wire_body.to_vec()),
        "zstd" => {
            let decoder = zstd::stream::read::Decoder::new(Cursor::new(wire_body))
                .map_err(|_| anyhow!("request body has invalid zstd encoding"))?;
            let mut body = Vec::new();
            decoder.take((128 << 20) + 1).read_to_end(&mut body)?;
            if body.len() > 128 << 20 {
                bail!("decoded request body is too large to validate");
            }
            Ok(body)
        }
        _ => bail!("request body content encoding is unsupported"),
    }
}

fn all_top_level_models(body: &[u8]) -> anyhow::Result<Vec<String>> {
    struct ModelsVisitor;
    impl<'de> Visitor<'de> for ModelsVisitor {
        type Value = Vec<String>;

        fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            formatter.write_str("a JSON object")
        }

        fn visit_map<A: MapAccess<'de>>(self, mut access: A) -> Result<Self::Value, A::Error> {
            let mut models = Vec::new();
            while let Some(key) = access.next_key::<String>()? {
                if key == "model" {
                    let value = access.next_value::<String>()?;
                    let model = normalize_model(&value)
                        .ok_or_else(|| serde::de::Error::custom("request body model is invalid"))?;
                    models.push(model);
                } else {
                    access.next_value::<IgnoredAny>()?;
                }
            }
            Ok(models)
        }
    }
    let mut deserializer = serde_json::Deserializer::from_slice(body);
    let models = deserializer
        .deserialize_map(ModelsVisitor)
        .map_err(|error| anyhow!("request body is not valid JSON: {error}"))?;
    deserializer
        .end()
        .map_err(|_| anyhow!("request body has trailing JSON"))?;
    Ok(models)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lease_tokens_are_jwt_shaped_and_private_binding_is_exact() {
        let store = LeaseStore::default();
        let lease = store
            .put(LeaseTemplate {
                scope_key: "scope".into(),
                organization_id: "org".into(),
                workspace_id: "workspace".into(),
                conversation_id: "conversation".into(),
                invocation_id: "invocation".into(),
                session_key: "session".into(),
                agent: "pi".into(),
                provider: Provider::Codex,
                account_id: "private-account".into(),
                auth_mode: AuthMode::Oauth,
                model: "gpt-5.4".into(),
                proxy_base_url: "http://localhost:31415".into(),
            })
            .unwrap();
        assert!(looks_like_token(lease.token()));
        assert_eq!(
            store.resolve(lease.token()).unwrap().account_id,
            "private-account"
        );
        assert!(matches!(
            store.resolve(&format!("{}x", lease.token())),
            Err(LeaseError::Invalid)
        ));
        let decoded = decode_segment(lease.token().split('.').nth(1).unwrap()).unwrap();
        assert!(
            !String::from_utf8(decoded)
                .unwrap()
                .contains("private-account")
        );
    }

    #[test]
    fn duplicate_model_fields_are_all_checked() {
        assert_eq!(
            all_top_level_models(br#"{"model":"a","nested":{"model":"ignored"},"model":"b"}"#)
                .unwrap(),
            ["a", "b"]
        );
    }
}
