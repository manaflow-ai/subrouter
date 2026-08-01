use std::collections::HashMap;
use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use bytes::Bytes;
use chrono::{DateTime, Utc};
use fs2::FileExt;
use http::{HeaderMap, Request};
use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};

const HEADER_CANDIDATES: &[&str] = &[
    "x-subrouter-session",
    "x-codex-window-id",
    "x-codex-turn-state",
    "x-codex-parent-thread-id",
    "x-session-id",
    "x-conversation-id",
    "x-codex-session-id",
    "x-claude-session-id",
    "x-claude-code-session-id",
    "x-gemini-session-id",
    "x-gemini-conversation-id",
    "openai-conversation-id",
    "anthropic-conversation-id",
    "google-conversation-id",
    "idempotency-key",
];
const USER_EMAIL_HEADERS: &[&str] = &["x-subrouter-user-email", "x-subrouter-user", "x-user-email"];
const ACCOUNT_ID_HEADERS: &[&str] = &["x-subrouter-account-id", "x-subrouter-account"];
const MODEL_HEADERS: &[&str] = &["x-subrouter-model", "x-model"];
const CODEX_AGENT_HEADERS: &[&str] = &[
    "x-codex-window-id",
    "x-codex-turn-state",
    "x-codex-parent-thread-id",
    "x-codex-session-id",
    "openai-conversation-id",
];
const CLAUDE_AGENT_HEADERS: &[&str] = &[
    "x-claude-session-id",
    "x-claude-code-session-id",
    "anthropic-conversation-id",
];
const GEMINI_AGENT_HEADERS: &[&str] = &[
    "x-gemini-session-id",
    "x-gemini-conversation-id",
    "google-conversation-id",
];
const SUBROUTER_HEADERS: &[&str] = &[
    "x-subrouter-lease",
    "x-subrouter-session",
    "x-subrouter-agent",
    "x-subrouter-user-email",
    "x-subrouter-user",
    "x-user-email",
    "x-subrouter-account-id",
    "x-subrouter-account",
    "x-subrouter-model",
    "x-model",
];

#[must_use]
pub fn extract_agent_type<B>(request: &Request<B>) -> String {
    if let Some(explicit) =
        header(request.headers(), "x-subrouter-agent").and_then(normalize_agent_type)
    {
        return explicit;
    }
    let user_agent = header(request.headers(), "user-agent")
        .unwrap_or_default()
        .to_ascii_lowercase();
    if user_agent.contains("claude-cli")
        || user_agent.contains("claude-code")
        || has_any_header(request.headers(), CLAUDE_AGENT_HEADERS)
    {
        return "claude".into();
    }
    if has_any_header(request.headers(), GEMINI_AGENT_HEADERS) {
        return "gemini".into();
    }
    if has_any_header(request.headers(), CODEX_AGENT_HEADERS) {
        return "codex".into();
    }
    "codex".into()
}

#[must_use]
pub fn normalize_agent_type(value: &str) -> Option<String> {
    let normalized = value.trim().to_ascii_lowercase();
    (!normalized.is_empty()
        && normalized.len() <= 64
        && normalized.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'.' | b'_' | b'-')
        }))
    .then_some(normalized)
}

#[must_use]
pub fn extract_user_email<B>(request: &Request<B>) -> Option<String> {
    USER_EMAIL_HEADERS
        .iter()
        .filter_map(|name| header(request.headers(), name))
        .find_map(normalize_user_email)
}

#[must_use]
pub fn extract_account_id<B>(request: &Request<B>) -> Option<String> {
    ACCOUNT_ID_HEADERS
        .iter()
        .filter_map(|name| header(request.headers(), name))
        .find_map(normalize_account_id)
}

#[must_use]
pub fn extract_model(request: &Request<Bytes>, max_body_bytes: usize) -> Option<String> {
    for name in MODEL_HEADERS {
        if let Some(model) = header(request.headers(), name).and_then(normalize_model) {
            return Some(model);
        }
    }
    if let Some(model) =
        query_value(request.uri().query(), "model").and_then(|value| normalize_model(&value))
    {
        return Some(model);
    }
    extract_json_model(request.headers(), request.body(), max_body_bytes)
}

#[must_use]
pub fn normalize_model(value: &str) -> Option<String> {
    let value = value.trim();
    (!value.is_empty() && value.len() <= 256).then(|| value.to_owned())
}

#[must_use]
pub fn normalize_user_email(value: &str) -> Option<String> {
    let value = value.trim();
    if value.is_empty() || value.len() > 320 {
        return None;
    }
    let address = match (value.rfind('<'), value.rfind('>')) {
        (Some(open), Some(close)) if open < close && value[close + 1..].trim().is_empty() => {
            value[open + 1..close].trim()
        }
        (None, None) => value,
        _ => return None,
    };
    if address
        .bytes()
        .any(|byte| byte.is_ascii_whitespace() || byte.is_ascii_control())
    {
        return None;
    }
    let (local, domain) = address.split_once('@')?;
    if local.is_empty()
        || domain.is_empty()
        || domain.contains('@')
        || domain.starts_with('.')
        || domain.ends_with('.')
    {
        return None;
    }
    Some(address.to_ascii_lowercase())
}

#[must_use]
pub fn normalize_account_id(value: &str) -> Option<String> {
    let value = value.trim();
    (!value.is_empty() && value.len() <= 256).then(|| value.to_owned())
}

pub fn strip_subrouter_headers(headers: &mut HeaderMap) {
    for name in SUBROUTER_HEADERS {
        headers.remove(*name);
    }
}

#[must_use]
pub fn extract_id(request: &Request<Bytes>, max_body_bytes: usize, remote_addr: &str) -> String {
    for name in HEADER_CANDIDATES {
        if let Some(value) = header(request.headers(), name)
            .map(str::trim)
            .filter(|value| !value.is_empty())
        {
            return value.to_owned();
        }
    }
    for name in ["session_id", "conversation_id", "thread_id"] {
        if let Some(value) = query_value(request.uri().query(), name)
            .map(|value| value.trim().to_owned())
            .filter(|value| !value.is_empty())
        {
            return value;
        }
    }
    if let Some(value) = extract_json_value(request.headers(), request.body(), max_body_bytes)
        && let Some(id) = find_json_id(&value)
    {
        return id;
    }
    fallback_id(request, remote_addr)
}

fn extract_json_model(headers: &HeaderMap, body: &Bytes, max_body_bytes: usize) -> Option<String> {
    if let Some(value) = extract_json_value(headers, body, max_body_bytes)
        && let Some(model) = find_json_model(&value)
    {
        return Some(model);
    }
    if !content_type_is_json(headers) || body.is_empty() || max_body_bytes == 0 {
        return None;
    }
    let scan_limit = max_body_bytes.max(8 << 20).min(body.len());
    let expression =
        Regex::new(r#""model"\s*:\s*"((?:\\.|[^"\\])*)""#).expect("static model regex");
    let text = std::str::from_utf8(&body[..scan_limit]).ok()?;
    let escaped = expression.captures(text)?.get(1)?.as_str();
    let decoded: String = serde_json::from_str(&format!("\"{escaped}\"")).ok()?;
    normalize_model(&decoded)
}

fn extract_json_value(headers: &HeaderMap, body: &Bytes, max_body_bytes: usize) -> Option<Value> {
    if body.is_empty()
        || max_body_bytes == 0
        || body.len() > max_body_bytes
        || !content_type_is_json(headers)
    {
        return None;
    }
    serde_json::from_slice(body).ok()
}

fn content_type_is_json(headers: &HeaderMap) -> bool {
    header(headers, "content-type").is_some_and(|value| value.contains("json"))
}

fn find_json_id(value: &Value) -> Option<String> {
    match value {
        Value::Object(object) => {
            for (key, value) in object {
                if matches!(
                    key.to_ascii_lowercase().as_str(),
                    "session_id" | "conversation_id" | "thread_id"
                ) && let Some(value) = value
                    .as_str()
                    .map(str::trim)
                    .filter(|value| !value.is_empty())
                {
                    return Some(value.to_owned());
                }
            }
            object.values().find_map(find_json_id)
        }
        Value::Array(values) => values.iter().find_map(find_json_id),
        _ => None,
    }
}

fn find_json_model(value: &Value) -> Option<String> {
    match value {
        Value::Object(object) => object
            .get("model")
            .and_then(Value::as_str)
            .and_then(normalize_model)
            .or_else(|| object.values().find_map(find_json_model)),
        Value::Array(values) => values.iter().find_map(find_json_model),
        _ => None,
    }
}

fn fallback_id(request: &Request<Bytes>, remote_addr: &str) -> String {
    let mut hash = Sha256::new();
    for value in [
        remote_addr,
        header(request.headers(), "user-agent").unwrap_or_default(),
        request.method().as_str(),
        request.uri().path(),
    ] {
        hash.update(value.as_bytes());
        if value != request.uri().path() {
            hash.update([0]);
        }
    }
    format!("fallback:{}", &hex::encode(hash.finalize())[..24])
}

fn query_value(query: Option<&str>, key: &str) -> Option<String> {
    url::form_urlencoded::parse(query?.as_bytes())
        .find_map(|(candidate, value)| (candidate == key).then(|| value.into_owned()))
}

fn has_any_header(headers: &HeaderMap, names: &[&str]) -> bool {
    names
        .iter()
        .any(|name| header(headers, name).is_some_and(|value| !value.trim().is_empty()))
}

fn header<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers.get(name)?.to_str().ok()
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Assignment {
    #[serde(default)]
    pub agent_type: String,
    #[serde(default)]
    pub session_id: String,
    pub account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub user_email: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

pub struct Store {
    path: PathBuf,
    data: Mutex<HashMap<String, Assignment>>,
}

impl Store {
    pub fn new(path: impl Into<PathBuf>) -> io::Result<Self> {
        let path = path.into();
        let _lock = FileLock::acquire(&path)?;
        let mut data = load(&path)?;
        migrate_assignments(&mut data);
        Ok(Self {
            path,
            data: Mutex::new(data),
        })
    }

    #[must_use]
    pub fn get(&self, agent_type: &str, session_id: &str) -> Option<Assignment> {
        let mut data = self
            .data
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Ok(_lock) = FileLock::acquire(&self.path)
            && let Ok(mut latest) = load(&self.path)
        {
            migrate_assignments(&mut latest);
            *data = latest;
        }
        data.get(&scoped_session_key(agent_type, session_id))
            .cloned()
    }

    pub fn put(
        &self,
        agent_type: &str,
        session_id: &str,
        account_id: &str,
        user_email: &str,
    ) -> io::Result<Assignment> {
        let mut data = self
            .data
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let _lock = FileLock::acquire(&self.path)?;
        *data = load(&self.path)?;
        migrate_assignments(&mut data);
        let now = Utc::now();
        let agent_type = normalize_agent_type(agent_type).unwrap_or_else(|| "codex".into());
        let session_id = sticky_session_id(&agent_type, session_id);
        let key = scoped_session_key(&agent_type, &session_id);
        let existing = data.get(&key);
        let assignment = Assignment {
            agent_type,
            session_id,
            account_id: account_id.into(),
            user_email: normalize_user_email(user_email)
                .or_else(|| {
                    existing
                        .map(|value| value.user_email.clone())
                        .filter(|value| !value.is_empty())
                })
                .unwrap_or_default(),
            created_at: existing.map_or(now, |value| value.created_at),
            updated_at: now,
        };
        data.insert(key, assignment.clone());
        save(&self.path, &data)?;
        Ok(assignment)
    }

    #[must_use]
    pub fn all(&self) -> Vec<Assignment> {
        self.reload_best_effort();
        self.data
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .values()
            .cloned()
            .collect()
    }

    #[must_use]
    pub fn count_by_account(&self) -> HashMap<String, usize> {
        self.reload_best_effort();
        let mut counts = HashMap::new();
        for assignment in self
            .data
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .values()
        {
            *counts.entry(assignment.account_id.clone()).or_default() += 1;
        }
        counts
    }

    fn reload_best_effort(&self) {
        let Ok(_lock) = FileLock::acquire(&self.path) else {
            return;
        };
        let Ok(mut latest) = load(&self.path) else {
            return;
        };
        migrate_assignments(&mut latest);
        *self
            .data
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = latest;
    }
}

struct FileLock(std::fs::File);

impl FileLock {
    fn acquire(path: &Path) -> io::Result<Self> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)?;
        }
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path.with_extension(format!(
                    "{}lock",
                    path.extension()
                        .map_or_else(String::new, |value| format!("{}.", value.to_string_lossy()))
                )))?;
        file.lock_exclusive()?;
        Ok(Self(file))
    }
}

impl Drop for FileLock {
    fn drop(&mut self) {
        let _ = self.0.unlock();
    }
}

fn load(path: &Path) -> io::Result<HashMap<String, Assignment>> {
    match fs::read(path) {
        Ok(body) => serde_json::from_slice(&body).map_err(io::Error::other),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(HashMap::new()),
        Err(error) => Err(error),
    }
}

fn save(path: &Path, data: &HashMap<String, Assignment>) -> io::Result<()> {
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let mut body = serde_json::to_vec_pretty(data).map_err(io::Error::other)?;
    body.push(b'\n');
    let mut temp = tempfile::NamedTempFile::new_in(parent)?;
    temp.write_all(&body)?;
    temp.as_file().sync_all()?;
    set_private_permissions(temp.path())?;
    temp.persist(path).map_err(|error| error.error)?;
    Ok(())
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

#[must_use]
pub fn default_store_path() -> PathBuf {
    std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".subrouter/sessions.json")
}

#[must_use]
pub fn scoped_session_key(agent_type: &str, session_id: &str) -> String {
    let agent_type = normalize_agent_type(agent_type).unwrap_or_else(|| "codex".into());
    format!(
        "{agent_type}:{}",
        sticky_session_id(&agent_type, session_id)
    )
}

#[must_use]
pub fn sticky_session_id(agent_type: &str, session_id: &str) -> String {
    if normalize_agent_type(agent_type).as_deref() == Some("codex") {
        base_session_id(session_id).into()
    } else {
        session_id.into()
    }
}

#[must_use]
pub fn base_session_id(session_id: &str) -> &str {
    session_id
        .split_once(':')
        .map_or(session_id, |(base, _)| base)
}

fn migrate_assignments(data: &mut HashMap<String, Assignment>) {
    let old = std::mem::take(data);
    for (key, mut assignment) in old {
        if assignment.session_id.is_empty() {
            assignment.session_id = key;
        }
        assignment.agent_type =
            normalize_agent_type(&assignment.agent_type).unwrap_or_else(|| "codex".into());
        assignment.session_id = sticky_session_id(&assignment.agent_type, &assignment.session_id);
        let next_key = scoped_session_key(&assignment.agent_type, &assignment.session_id);
        if data
            .get(&next_key)
            .is_some_and(|existing| existing.updated_at > assignment.updated_at)
        {
            continue;
        }
        data.insert(next_key, assignment);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(body: &str) -> Request<Bytes> {
        Request::builder()
            .method("POST")
            .uri("/v1/responses")
            .header("content-type", "application/json")
            .body(Bytes::copy_from_slice(body.as_bytes()))
            .unwrap()
    }

    #[test]
    fn extracts_request_metadata() {
        let mut request = request(r#"{"metadata":{"session_id":"s1"},"model":"gpt-5"}"#);
        request.headers_mut().insert(
            "x-subrouter-user-email",
            "Alice <Alice@Example.COM>".parse().unwrap(),
        );
        assert_eq!(extract_id(&request, 1024, "127.0.0.1:1"), "s1");
        assert_eq!(extract_model(&request, 1024).as_deref(), Some("gpt-5"));
        assert_eq!(
            extract_user_email(&request).as_deref(),
            Some("alice@example.com")
        );
    }

    #[test]
    fn scans_model_past_small_json_limit() {
        let body = format!(
            r#"{{"tools":[{{"input_schema":"{}"}}],"model":"claude-fable-5"}}"#,
            "x".repeat(2048)
        );
        assert_eq!(
            extract_model(&request(&body), 128).as_deref(),
            Some("claude-fable-5")
        );
    }

    #[test]
    fn store_merges_generations_and_scopes_agents() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("sessions.json");
        let old = Store::new(&path).unwrap();
        let new = Store::new(&path).unwrap();
        old.put("codex", "session:4", "codex-a", "Alice@Example.COM")
            .unwrap();
        new.put("claude", "session:4", "claude-a", "").unwrap();
        assert_eq!(
            old.get("claude", "session:4").unwrap().account_id,
            "claude-a"
        );
        assert_eq!(
            old.get("codex", "session:7").unwrap().user_email,
            "alice@example.com"
        );
        assert_eq!(Store::new(&path).unwrap().all().len(), 2);
    }
}
