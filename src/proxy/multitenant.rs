use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::body::{Body, to_bytes};
use axum::extract::{ConnectInfo, State};
use axum::response::{IntoResponse, Response};
use axum::routing::any;
use chrono::{DateTime, Utc};
use http::{HeaderMap, Method, Request, StatusCode, Uri};
use serde::Serialize;
use serde_json::json;
use tracing::warn;

use super::{AccountRef, Server};
use crate::account::AuthMode;
use crate::accounts::CodexStore;
use crate::agents::claude;
use crate::front::ClientAddr;
use crate::selectacct::{Scheduler, SchedulerRef, Score};
use crate::{session, tenant, transcript};

const ADMIN_BODY_LIMIT: usize = 1 << 16;

/// Resolves tenant keys before delegating to a server whose credentials,
/// sticky assignments, usage state, and transcripts are tenant-local.
pub struct MultiTenant {
    base: Arc<Server>,
    registry: Arc<tenant::Registry>,
    transcript_dir: Option<PathBuf>,
    enabled: bool,
    servers: Mutex<HashMap<String, Arc<Server>>>,
}

impl MultiTenant {
    #[must_use]
    pub fn new(
        base: Arc<Server>,
        registry: Arc<tenant::Registry>,
        transcript_dir: Option<PathBuf>,
        enabled: bool,
    ) -> Self {
        Self {
            base,
            registry,
            transcript_dir,
            enabled,
            servers: Mutex::new(HashMap::new()),
        }
    }

    pub fn router(self: Arc<Self>) -> Router {
        Router::new().fallback(any(route_request)).with_state(self)
    }

    async fn handle(self: Arc<Self>, mut request: Request<Body>) -> Response {
        let path = request.uri().path().to_owned();
        if path == "/_subrouter/tenants" || path.starts_with("/_subrouter/tenants/") {
            return self.handle_admin(request).await;
        }
        if let Some((key, rest)) = split_tenant_path(&path) {
            let tenant = match self.registry.resolve(key) {
                Ok(Some(value)) => value,
                Ok(None) => return text(StatusCode::UNAUTHORIZED, "unknown tenant key\n"),
                Err(error) => {
                    warn!(%error, "tenant registry resolution failed");
                    return text(StatusCode::INTERNAL_SERVER_ERROR, "tenant registry error\n");
                }
            };
            if rewrite_path(&mut request, rest).is_err() {
                return text(StatusCode::BAD_REQUEST, "invalid tenant path\n");
            }
            strip_tenant_credentials(request.headers_mut());
            return self.handle_tenant(tenant, request).await;
        }
        if let Some(key) = tenant_key_from_headers(request.headers()) {
            match self.registry.resolve(key) {
                Ok(Some(value)) => {
                    strip_tenant_credentials(request.headers_mut());
                    return self.handle_tenant(value, request).await;
                }
                Ok(None) if self.enabled || self.registry.has_tenants() => {
                    return text(StatusCode::UNAUTHORIZED, "unknown tenant key\n");
                }
                Ok(None) => {}
                Err(error) => {
                    warn!(%error, "tenant registry resolution failed");
                    return text(StatusCode::INTERNAL_SERVER_ERROR, "tenant registry error\n");
                }
            }
        }

        let remote = request
            .extensions()
            .get::<ConnectInfo<ClientAddr>>()
            .and_then(|value| value.0.0);
        let reload_tenants = request.method() == Method::POST
            && request.uri().path() == "/_subrouter/reload-accounts"
            && remote.is_none_or(|value| value.ip().is_loopback());
        let response = Arc::clone(&self.base).handle(request).await;
        if reload_tenants && response.status().is_success() {
            self.reload_tenant_accounts().await;
        }
        response
    }

    async fn handle_tenant(
        self: Arc<Self>,
        tenant: tenant::Tenant,
        request: Request<Body>,
    ) -> Response {
        let path = request.uri().path();
        if path == "/_subrouter/whoami" {
            return json_response(
                StatusCode::OK,
                &json!({"tenant_id": tenant.id, "name": tenant.name}),
            );
        }
        if path.starts_with("/_subrouter/") && !tenant_control_path(path) {
            return text(StatusCode::NOT_FOUND, "404 page not found\n");
        }
        match self.server_for(&tenant).await {
            Ok(server) => server.handle(request).await,
            Err(error) => {
                warn!(tenant = %tenant.id, %error, "tenant server initialization failed");
                text(StatusCode::INTERNAL_SERVER_ERROR, "tenant unavailable\n")
            }
        }
    }

    async fn server_for(&self, tenant: &tenant::Tenant) -> anyhow::Result<Arc<Server>> {
        if let Some(server) = self
            .servers
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(&tenant.id)
            .cloned()
        {
            return Ok(server);
        }
        let server = self.new_tenant_server(tenant).await?;
        let mut servers = self
            .servers
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        Ok(servers
            .entry(tenant.id.clone())
            .or_insert_with(|| Arc::clone(&server))
            .clone())
    }

    async fn new_tenant_server(&self, tenant: &tenant::Tenant) -> anyhow::Result<Arc<Server>> {
        let dir = self.registry.dir(&tenant.id);
        let codex_store = CodexStore::new(dir.join("codex/accounts"));
        let claude_store = claude::Store::new(dir.join("codex"));
        let sessions = Arc::new(session::Store::new(dir.join("sessions.json"))?);
        let mut accounts = codex_store.list()?;
        match claude_store.list_accounts().await {
            Ok(values) => accounts.extend(values),
            Err(error) => warn!(tenant = %tenant.id, %error, "tenant Claude accounts skipped"),
        }
        let scheduler = Arc::new(SchedulerRef::new(Scheduler::new(accounts.iter().map(
            |account| {
                let headroom = if account.auth_mode == AuthMode::ApiKey {
                    0.01
                } else {
                    1.0
                };
                Score {
                    account_id: account.id.clone(),
                    provider: account.provider,
                    headroom,
                    short_headroom: headroom,
                    ..Score::default()
                }
            },
        ))));
        let mut server = Server::new(sessions, scheduler)?;
        server.upstreams = self.base.upstreams.clone();
        server.account_ref = Some(AccountRef::new(
            codex_store,
            claude_store,
            accounts,
            reqwest::Client::builder()
                .timeout(std::time::Duration::from_secs(15))
                .build()?,
        ));
        server.credential_broker = self.base.credential_broker.clone();
        server.lifecycle = Arc::clone(&self.base.lifecycle);
        server.stream_drops = Arc::clone(&self.base.stream_drops);
        server.require_session_leases = self.base.require_session_leases;
        server.forward_session_headers = self.base.forward_session_headers;
        server.local_proxy_token = self.base.local_proxy_token.clone();
        server.max_body_bytes = self.base.max_body_bytes;
        server.usage_score_ttl = self.base.usage_score_ttl;
        server.claude_fable_api_key = self.base.claude_fable_api_key.clone();
        server.fable_bedrock_primary = self.base.fable_bedrock_primary;
        server.bedrock = self.base.bedrock.clone();
        // Possession of the tenant key authorizes the tenant-visible control
        // allowlist. Global administrative endpoints never reach this server.
        server.admin_token.clear();
        if let Some(root) = &self.transcript_dir {
            server.transcripts =
                transcript::Recorder::new(root.join("tenants").join(&tenant.id)).map(Arc::new);
        }
        Ok(Arc::new(server))
    }

    async fn reload_tenant_accounts(&self) {
        let servers: Vec<_> = self
            .servers
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .values()
            .cloned()
            .collect();
        for server in servers {
            if let Some(reference) = &server.account_ref
                && let Err(error) = reference.reload().await
            {
                warn!(%error, "tenant account reload failed");
            }
        }
    }

    async fn handle_admin(&self, request: Request<Body>) -> Response {
        let remote = request
            .extensions()
            .get::<ConnectInfo<ClientAddr>>()
            .and_then(|value| value.0.0);
        if !self.base.authorize_admin(remote, request.headers()) {
            return text(StatusCode::UNAUTHORIZED, "admin token required\n");
        }
        let method = request.method().clone();
        let path = request.uri().path().to_owned();
        let relative = path
            .trim_start_matches("/_subrouter/tenants")
            .trim_matches('/');
        if relative.is_empty() {
            return match method {
                Method::GET => match self.registry.list() {
                    Ok(values) => {
                        let views = values.iter().map(TenantView::from).collect::<Vec<_>>();
                        json_response(StatusCode::OK, &views)
                    }
                    Err(error) => text(StatusCode::INTERNAL_SERVER_ERROR, &format!("{error}\n")),
                },
                Method::POST => {
                    let body = match to_bytes(request.into_body(), ADMIN_BODY_LIMIT).await {
                        Ok(value) => value,
                        Err(_) => return text(StatusCode::BAD_REQUEST, "invalid JSON body\n"),
                    };
                    let name = serde_json::from_slice::<serde_json::Value>(&body)
                        .ok()
                        .and_then(|value| {
                            value
                                .get("name")
                                .and_then(serde_json::Value::as_str)
                                .map(str::to_owned)
                        });
                    let Some(name) = name else {
                        return text(StatusCode::BAD_REQUEST, "invalid JSON body\n");
                    };
                    match self.registry.create(&name) {
                        Ok((created, key)) => json_response(
                            StatusCode::OK,
                            &json!({"tenant": TenantView::from(&created), "key": key}),
                        ),
                        Err(error) => text(StatusCode::BAD_REQUEST, &format!("{error}\n")),
                    }
                }
                _ => text(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n"),
            };
        }
        let parts: Vec<_> = relative.split('/').collect();
        match parts.as_slice() {
            [tenant_id, "keys"] if method == Method::POST => {
                match self.registry.create_key(tenant_id) {
                    Ok((updated, key)) => json_response(
                        StatusCode::OK,
                        &json!({"tenant": TenantView::from(&updated), "key": key}),
                    ),
                    Err(error) => text(StatusCode::NOT_FOUND, &format!("{error}\n")),
                }
            }
            [tenant_id, "keys", prefix] if method == Method::DELETE => match self
                .registry
                .revoke_key(tenant_id, prefix)
            {
                Ok(revoked) => json_response(StatusCode::OK, &json!({"ok":true,"revoked":revoked})),
                Err(error) => text(StatusCode::NOT_FOUND, &format!("{error}\n")),
            },
            _ => text(StatusCode::NOT_FOUND, "404 page not found\n"),
        }
    }
}

async fn route_request(State(router): State<Arc<MultiTenant>>, request: Request<Body>) -> Response {
    router.handle(request).await
}

fn split_tenant_path(path: &str) -> Option<(&str, &str)> {
    let remainder = path.strip_prefix("/t/")?;
    match remainder.split_once('/') {
        Some((key, rest)) => Some((
            key,
            if rest.is_empty() {
                "/"
            } else {
                &path[path.len() - rest.len() - 1..]
            },
        )),
        None => Some((remainder, "/")),
    }
}

fn tenant_key_from_headers(headers: &HeaderMap) -> Option<&str> {
    if let Some(value) = headers
        .get("x-api-key")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|value| tenant::valid_key_format(value))
    {
        return Some(value);
    }
    let authorization = headers
        .get(http::header::AUTHORIZATION)?
        .to_str()
        .ok()?
        .trim();
    let (scheme, token) = authorization.split_once(' ')?;
    (scheme.eq_ignore_ascii_case("bearer") && tenant::valid_key_format(token.trim()))
        .then(|| token.trim())
}

fn strip_tenant_credentials(headers: &mut HeaderMap) {
    if headers
        .get("x-api-key")
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| tenant::valid_key_format(value.trim()))
    {
        headers.remove("x-api-key");
    }
    if headers
        .get(http::header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.trim().split_once(' '))
        .is_some_and(|(scheme, token)| {
            scheme.eq_ignore_ascii_case("bearer") && tenant::valid_key_format(token.trim())
        })
    {
        headers.remove(http::header::AUTHORIZATION);
    }
}

fn rewrite_path(request: &mut Request<Body>, path: &str) -> anyhow::Result<()> {
    let query = request
        .uri()
        .query()
        .map(|value| format!("?{value}"))
        .unwrap_or_default();
    let path_and_query = format!("{path}{query}").parse()?;
    let mut parts = request.uri().clone().into_parts();
    parts.path_and_query = Some(path_and_query);
    *request.uri_mut() = Uri::from_parts(parts)?;
    Ok(())
}

fn tenant_control_path(path: &str) -> bool {
    matches!(
        path,
        "/_subrouter/health"
            | "/_subrouter/accounts"
            | "/_subrouter/account-status"
            | "/_subrouter/usage-status"
            | "/_subrouter/sessions"
            | "/_subrouter/reload-accounts"
    )
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct TenantKeyView<'a> {
    prefix: &'a str,
    created_at: DateTime<Utc>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct TenantView<'a> {
    id: &'a str,
    name: &'a str,
    created_at: DateTime<Utc>,
    keys: Vec<TenantKeyView<'a>>,
}

impl<'a> From<&'a tenant::Tenant> for TenantView<'a> {
    fn from(value: &'a tenant::Tenant) -> Self {
        Self {
            id: &value.id,
            name: &value.name,
            created_at: value.created_at,
            keys: value
                .keys
                .iter()
                .map(|key| TenantKeyView {
                    prefix: &key.prefix,
                    created_at: key.created_at,
                })
                .collect(),
        }
    }
}

fn json_response(status: StatusCode, value: &impl Serialize) -> Response {
    let body = serde_json::to_vec(value).unwrap_or_else(|_| b"null".to_vec());
    (
        status,
        [
            (http::header::CONTENT_TYPE, "application/json"),
            (http::header::CACHE_CONTROL, "no-store"),
        ],
        body,
    )
        .into_response()
}

fn text(status: StatusCode, value: &str) -> Response {
    (status, value.to_owned()).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn path_and_header_key_parsing_is_exact() {
        assert_eq!(
            split_tenant_path("/t/srt_key/v1/responses"),
            Some(("srt_key", "/v1/responses"))
        );
        assert_eq!(split_tenant_path("/t/srt_key"), Some(("srt_key", "/")));
        assert_eq!(split_tenant_path("/v1/responses"), None);
        let mut headers = HeaderMap::new();
        headers.insert(
            http::header::AUTHORIZATION,
            "Bearer srt_00000000000000000000000000000000"
                .parse()
                .unwrap(),
        );
        assert_eq!(
            tenant_key_from_headers(&headers),
            Some("srt_00000000000000000000000000000000")
        );
        strip_tenant_credentials(&mut headers);
        assert!(!headers.contains_key(http::header::AUTHORIZATION));
    }

    #[tokio::test]
    async fn admin_views_never_expose_key_hashes() {
        use tower::ServiceExt as _;

        let root = tempfile::tempdir().unwrap();
        let registry = Arc::new(tenant::Registry::new(root.path()));
        let (_, plaintext) = registry.create("acme").unwrap();
        let sessions =
            Arc::new(session::Store::new(root.path().join("base-sessions.json")).unwrap());
        let base = Arc::new(
            Server::new(sessions, Arc::new(SchedulerRef::new(Scheduler::default()))).unwrap(),
        );
        let app = Arc::new(MultiTenant::new(base, Arc::clone(&registry), None, false)).router();
        let response = app
            .oneshot(
                Request::get("/_subrouter/tenants")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let body = to_bytes(response.into_body(), 1 << 20).await.unwrap();
        let text = String::from_utf8(body.to_vec()).unwrap();
        assert!(!text.contains(&plaintext));
        assert!(!text.contains(&tenant::hash_key(&plaintext)));
        assert!(text.contains("acme"));
    }
}
