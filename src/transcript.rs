use std::collections::{BTreeMap, HashMap};
use std::fs::{self, File, OpenOptions};
use std::io::{self, BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use base64::{Engine, engine::general_purpose::STANDARD as BASE64};
use chrono::{DateTime, Duration, SecondsFormat, TimeZone, Utc};
use http::HeaderMap;
use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};

pub mod gcs;

const MAX_JSONL_LINE_BYTES: usize = 32 * 1024 * 1024;

#[derive(Clone, Deserialize, Serialize)]
pub struct Event {
    pub timestamp: String,
    #[serde(rename = "type")]
    pub event_type: String,
    pub payload: Map<String, Value>,
}

#[derive(Debug)]
pub struct Recorder {
    dir: PathBuf,
    write_lock: Mutex<()>,
}

impl Recorder {
    #[must_use]
    pub fn new(dir: impl Into<PathBuf>) -> Option<Self> {
        let dir = dir.into();
        (!dir.as_os_str().is_empty()).then_some(Self {
            dir,
            write_lock: Mutex::new(()),
        })
    }

    #[must_use]
    pub fn enabled(&self) -> bool {
        !self.dir.as_os_str().is_empty()
    }

    #[must_use]
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    pub fn record_meta(&self, agent_type: &str, session_id: &str, payload: Map<String, Value>) {
        self.write(
            agent_type,
            session_id,
            Event {
                timestamp: now(),
                event_type: "subrouter_meta".into(),
                payload: with_session(agent_type, session_id, payload),
            },
        );
    }

    pub fn record_payload(
        &self,
        agent_type: &str,
        session_id: &str,
        event_type: &str,
        direction: &str,
        body: &[u8],
        payload: Map<String, Value>,
    ) {
        let mut payload = with_session(agent_type, session_id, payload);
        payload.insert("direction".into(), direction.into());
        payload.insert("bytes".into(), body.len().into());
        payload.insert("sha256".into(), hex::encode(Sha256::digest(body)).into());
        payload.insert("body_base64".into(), BASE64.encode(body).into());
        self.write(
            agent_type,
            session_id,
            Event {
                timestamp: now(),
                event_type: event_type.into(),
                payload,
            },
        );
    }

    #[allow(clippy::too_many_arguments)]
    pub fn record_payload_chunk(
        &self,
        agent_type: &str,
        session_id: &str,
        event_type: &str,
        direction: &str,
        stream_id: &str,
        chunk_index: usize,
        offset: u64,
        body: &[u8],
        payload: Map<String, Value>,
    ) {
        let mut payload = with_session(agent_type, session_id, payload);
        payload.extend([
            ("direction".into(), direction.into()),
            ("stream_id".into(), stream_id.into()),
            ("body_chunk".into(), true.into()),
            ("chunk_index".into(), chunk_index.into()),
            ("offset".into(), offset.into()),
            ("chunk_bytes".into(), body.len().into()),
            (
                "chunk_sha256".into(),
                hex::encode(Sha256::digest(body)).into(),
            ),
            ("body_base64".into(), BASE64.encode(body).into()),
        ]);
        self.write(
            agent_type,
            session_id,
            Event {
                timestamp: now(),
                event_type: format!("{event_type}_chunk"),
                payload,
            },
        );
    }

    #[allow(clippy::too_many_arguments)]
    pub fn record_payload_summary(
        &self,
        agent_type: &str,
        session_id: &str,
        event_type: &str,
        direction: &str,
        stream_id: &str,
        bytes_read: u64,
        sha256: &str,
        chunks: usize,
        payload: Map<String, Value>,
    ) {
        let mut payload = with_session(agent_type, session_id, payload);
        payload.extend([
            ("direction".into(), direction.into()),
            ("stream_id".into(), stream_id.into()),
            ("body_chunked".into(), true.into()),
            ("bytes".into(), bytes_read.into()),
            ("sha256".into(), sha256.into()),
            ("chunks".into(), chunks.into()),
        ]);
        self.write(
            agent_type,
            session_id,
            Event {
                timestamp: now(),
                event_type: event_type.into(),
                payload,
            },
        );
    }

    #[must_use]
    pub fn path_for_session(&self, agent_type: &str, session_id: &str) -> PathBuf {
        session_path(&self.dir, agent_type, session_id)
    }

    fn write(&self, agent_type: &str, session_id: &str, event: Event) {
        let Ok(mut body) = serde_json::to_vec(&event) else {
            return;
        };
        body.push(b'\n');
        let path = self.path_for_session(agent_type, session_id);
        let _guard = self
            .write_lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let Some(parent) = path.parent() else { return };
        if fs::create_dir_all(parent).is_err() {
            return;
        }
        let Ok(mut file) = open_append_private(&path) else {
            return;
        };
        let _ = file.write_all(&body);
    }
}

#[must_use]
pub fn base_session_id(session_id: &str) -> &str {
    session_id
        .split_once(':')
        .map_or(session_id, |(base, _)| base)
}

#[must_use]
pub fn redacted_headers(headers: &HeaderMap) -> BTreeMap<String, Vec<String>> {
    let mut output = BTreeMap::new();
    for (name, value) in headers {
        let values = output
            .entry(name.as_str().to_owned())
            .or_insert_with(Vec::new);
        if sensitive_header(name.as_str()) {
            if values.is_empty() {
                values.push("<redacted>".into());
            }
        } else {
            values.push(value.to_str().unwrap_or("<non-utf8>").into());
        }
    }
    output
}

fn with_session(
    agent_type: &str,
    session_id: &str,
    mut payload: Map<String, Value>,
) -> Map<String, Value> {
    let agent_type = normalize_agent_type(agent_type);
    let agent_session_id = base_session_id(session_id);
    payload.insert("agent_type".into(), agent_type.clone().into());
    payload.insert("session_id".into(), session_id.into());
    payload.insert("agent_session_id".into(), agent_session_id.into());
    if agent_type == "codex" {
        payload.insert("codex_session_id".into(), agent_session_id.into());
    }
    payload
}

fn normalize_agent_type(agent_type: &str) -> String {
    let normalized = agent_type.trim().to_ascii_lowercase();
    if normalized.is_empty() {
        "codex".into()
    } else {
        normalized
    }
}

fn now() -> String {
    Utc::now().to_rfc3339_opts(SecondsFormat::Nanos, true)
}

fn sensitive_header(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "authorization"
            | "cookie"
            | "set-cookie"
            | "proxy-authorization"
            | "x-api-key"
            | "openai-api-key"
    )
}

fn safe_filename(value: &str) -> String {
    if value.is_empty() {
        return "unknown".into();
    }
    let expression = Regex::new(r"[^A-Za-z0-9._-]+").expect("static filename regex");
    let safe = expression.replace_all(value, "_");
    let safe = safe.trim_matches(['.', '_', '-']);
    if safe.is_empty() {
        "unknown".into()
    } else {
        safe.into()
    }
}

fn open_append_private(path: &Path) -> io::Result<File> {
    let mut options = OpenOptions::new();
    options.create(true).append(true).write(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    options.open(path)
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Usage {
    pub requests: i64,
    pub input_tokens: i64,
    pub cached_input_tokens: i64,
    pub output_tokens: i64,
    pub reasoning_tokens: i64,
    pub total_tokens: i64,
    pub payload_bytes: i64,
}

impl Usage {
    fn add(&mut self, other: &Self) {
        self.requests += other.requests;
        self.input_tokens += other.input_tokens;
        self.cached_input_tokens += other.cached_input_tokens;
        self.output_tokens += other.output_tokens;
        self.reasoning_tokens += other.reasoning_tokens;
        self.total_tokens += other.total_tokens;
        self.payload_bytes += other.payload_bytes;
    }
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Summary {
    pub agent_type: String,
    pub session_id: String,
    pub event_count: usize,
    pub total_bytes: i64,
    pub usage: Usage,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub first_event_at: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub last_event_at: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub user: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub account: String,
    pub has_bodies: bool,
    pub size_bytes: u64,
}

#[derive(Clone, Deserialize, Serialize)]
pub struct SanitizedEvent {
    pub timestamp: String,
    #[serde(rename = "type")]
    pub event_type: String,
    pub payload: Map<String, Value>,
}

pub fn list_summaries(dir: &Path) -> io::Result<Vec<Summary>> {
    if dir.as_os_str().is_empty() {
        return Ok(Vec::new());
    }
    let root = dir.join("by-agent");
    let mut files = Vec::new();
    collect_jsonl(&root, &mut files)?;
    let mut summaries = files
        .into_iter()
        .map(|path| summarize_file(&root, &path))
        .collect::<io::Result<Vec<_>>>()?;
    summaries.sort_by(|left, right| {
        right
            .last_event_at
            .cmp(&left.last_event_at)
            .then_with(|| left.session_id.cmp(&right.session_id))
    });
    Ok(summaries)
}

pub fn read_sanitized_session(
    dir: &Path,
    agent_type: &str,
    session_id: &str,
) -> io::Result<Vec<SanitizedEvent>> {
    read_events(&session_path(dir, agent_type, session_id))?
        .into_iter()
        .map(|event| {
            let mut payload = event.payload;
            if payload.remove("body_base64").is_some() {
                payload.insert("body_base64_redacted".into(), true.into());
            }
            Ok(SanitizedEvent {
                timestamp: event.timestamp,
                event_type: event.event_type,
                payload,
            })
        })
        .collect()
}

#[must_use]
pub fn session_path(dir: &Path, agent_type: &str, session_id: &str) -> PathBuf {
    dir.join("by-agent")
        .join(safe_filename(&normalize_agent_type(agent_type)))
        .join("by-session")
        .join(format!(
            "{}.jsonl",
            safe_filename(base_session_id(session_id))
        ))
}

fn collect_jsonl(root: &Path, output: &mut Vec<PathBuf>) -> io::Result<()> {
    let entries = match fs::read_dir(root) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error),
    };
    for entry in entries {
        let entry = entry?;
        if entry.file_type()?.is_dir() {
            collect_jsonl(&entry.path(), output)?;
        } else if entry
            .path()
            .extension()
            .is_some_and(|extension| extension == "jsonl")
        {
            output.push(entry.path());
        }
    }
    Ok(())
}

fn summarize_file(root: &Path, path: &Path) -> io::Result<Summary> {
    let relative = path.strip_prefix(root).map_err(io::Error::other)?;
    let parts: Vec<_> = relative.components().collect();
    let mut summary = Summary {
        agent_type: parts.first().map_or_else(String::new, |part| {
            part.as_os_str().to_string_lossy().into()
        }),
        session_id: path
            .file_stem()
            .map_or_else(String::new, |part| part.to_string_lossy().into()),
        size_bytes: fs::metadata(path)?.len(),
        ..Summary::default()
    };
    let mut chunks = BodyChunkAccumulator::default();
    for event in read_events(path)? {
        apply_event(&mut summary, &event);
        if let Some(body) = chunks.body_from_event(&event) {
            for record in extract_usage_records(&body) {
                summary.usage.add(&record.usage);
            }
        }
    }
    Ok(summary)
}

fn apply_event(summary: &mut Summary, event: &Event) {
    summary.event_count += 1;
    if !event.timestamp.is_empty() {
        if summary.first_event_at.is_empty()
            || timestamp_less(&event.timestamp, &summary.first_event_at)
        {
            summary.first_event_at.clone_from(&event.timestamp);
        }
        if summary.last_event_at.is_empty()
            || timestamp_less(&summary.last_event_at, &event.timestamp)
        {
            summary.last_event_at.clone_from(&event.timestamp);
        }
    }
    if summary.agent_type.is_empty() {
        summary.agent_type = string_field(&event.payload, "agent_type")
            .unwrap_or_default()
            .into();
    }
    if summary.session_id.is_empty() {
        summary.session_id = string_field(&event.payload, "agent_session_id")
            .unwrap_or_default()
            .into();
    }
    if summary.user.is_empty() {
        summary.user = string_field(&event.payload, "user")
            .unwrap_or_default()
            .into();
    }
    if summary.account.is_empty() {
        summary.account = string_field(&event.payload, "account")
            .unwrap_or_default()
            .into();
    }
    summary.total_bytes += number_field(&event.payload, &["bytes"]);
    summary.has_bodies |= event.payload.contains_key("body_base64")
        || bool_field(&event.payload, "body_chunked")
        || bool_field(&event.payload, "body_chunk");
}

fn timestamp_less(left: &str, right: &str) -> bool {
    match (
        DateTime::parse_from_rfc3339(left),
        DateTime::parse_from_rfc3339(right),
    ) {
        (Ok(left), Ok(right)) => left < right,
        _ => left < right,
    }
}

fn read_events(path: &Path) -> io::Result<Vec<Event>> {
    let file = File::open(path)?;
    let mut events = Vec::new();
    for line in BufReader::new(file).split(b'\n') {
        let line = line?;
        if line.is_empty() {
            continue;
        }
        if line.len() > MAX_JSONL_LINE_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "transcript line exceeds 32 MiB",
            ));
        }
        events.push(serde_json::from_slice(&line).map_err(io::Error::other)?);
    }
    Ok(events)
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct UsageGroup {
    pub key: String,
    pub usage: Usage,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct UsageBucket {
    pub start: String,
    pub label: String,
    pub usage: Usage,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Analytics {
    pub totals: Usage,
    pub by_user: Vec<UsageGroup>,
    pub by_account: Vec<UsageGroup>,
    pub by_model: Vec<UsageGroup>,
    pub timeline: Vec<UsageBucket>,
    pub max_timeline_tokens: i64,
    pub max_user_tokens: i64,
    pub max_account_tokens: i64,
}

#[derive(Clone, Serialize)]
pub struct RawEvent {
    pub timestamp: String,
    #[serde(rename = "type")]
    pub event_type: String,
    pub payload: Map<String, Value>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_text: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_base64: String,
}

#[derive(Clone, Debug, Default)]
struct UsageRecord {
    timestamp: Option<DateTime<Utc>>,
    user: String,
    account: String,
    model: String,
    usage: Usage,
}

pub fn analyze(dir: &Path) -> io::Result<Analytics> {
    Ok(build_analytics(&read_usage_records(dir)?))
}

pub fn read_raw_session(
    dir: &Path,
    agent_type: &str,
    session_id: &str,
) -> io::Result<Vec<RawEvent>> {
    let mut chunks = BodyChunkAccumulator::default();
    Ok(read_events(&session_path(dir, agent_type, session_id))?
        .into_iter()
        .map(|event| {
            let body = chunks.body_from_event(&event);
            let mut payload = event.payload;
            payload.remove("body_base64");
            let (body_text, body_base64) = match body {
                Some(body) => match String::from_utf8(body) {
                    Ok(text) => (text, String::new()),
                    Err(error) => (String::new(), BASE64.encode(error.into_bytes())),
                },
                None => (String::new(), String::new()),
            };
            RawEvent {
                timestamp: event.timestamp,
                event_type: event.event_type,
                payload,
                body_text,
                body_base64,
            }
        })
        .collect())
}

fn read_usage_records(dir: &Path) -> io::Result<Vec<UsageRecord>> {
    if dir.as_os_str().is_empty() {
        return Ok(Vec::new());
    }
    let mut files = Vec::new();
    collect_jsonl(&dir.join("by-agent"), &mut files)?;
    let mut output = Vec::new();
    for path in files {
        output.extend(usage_records_from_file(&path)?);
    }
    Ok(output)
}

fn usage_records_from_file(path: &Path) -> io::Result<Vec<UsageRecord>> {
    let mut output = Vec::new();
    let mut user = String::new();
    let mut account = String::new();
    let mut chunks = BodyChunkAccumulator::default();
    for event in read_events(path)? {
        if let Some(value) = string_field(&event.payload, "user") {
            user = value.into();
        }
        if let Some(value) = string_field(&event.payload, "account") {
            account = value.into();
        }
        let Some(body) = chunks.body_from_event(&event) else {
            continue;
        };
        let timestamp = DateTime::parse_from_rfc3339(&event.timestamp)
            .ok()
            .map(|value| value.with_timezone(&Utc));
        for mut record in extract_usage_records(&body) {
            record.timestamp = timestamp;
            if record.user.is_empty() {
                record.user.clone_from(&user);
            }
            if record.account.is_empty() {
                record.account.clone_from(&account);
            }
            record.usage.payload_bytes = number_field(&event.payload, &["bytes"]);
            output.push(record);
        }
    }
    Ok(output)
}

#[derive(Default)]
struct BodyChunkAccumulator {
    streams: HashMap<String, BTreeMap<i64, Vec<u8>>>,
}

impl BodyChunkAccumulator {
    fn body_from_event(&mut self, event: &Event) -> Option<Vec<u8>> {
        if let Some(encoded) = string_field(&event.payload, "body_base64") {
            let body = BASE64.decode(encoded).ok()?;
            if is_body_chunk(event) {
                let stream = string_field(&event.payload, "stream_id")?;
                let default_index = self.streams.get(stream).map_or(0, BTreeMap::len) as i64;
                let index = event
                    .payload
                    .get("chunk_index")
                    .and_then(Value::as_i64)
                    .unwrap_or(default_index);
                self.streams
                    .entry(stream.into())
                    .or_default()
                    .insert(index, body);
                return None;
            }
            return Some(body);
        }
        if bool_field(&event.payload, "body_chunked") {
            let stream = string_field(&event.payload, "stream_id")?;
            let chunks = self.streams.remove(stream)?;
            return Some(chunks.into_values().flatten().collect());
        }
        None
    }
}

fn is_body_chunk(event: &Event) -> bool {
    bool_field(&event.payload, "body_chunk") || event.event_type.ends_with("_chunk")
}

fn extract_usage_records(body: &[u8]) -> Vec<UsageRecord> {
    json_payloads(body)
        .into_iter()
        .filter_map(|payload| usage_record_from_payload(&payload))
        .collect()
}

fn json_payloads(body: &[u8]) -> Vec<Map<String, Value>> {
    if let Ok(payload) = serde_json::from_slice(body) {
        return vec![payload];
    }
    body.split(|byte| *byte == b'\n')
        .filter_map(|line| {
            let line = std::str::from_utf8(line)
                .ok()?
                .trim()
                .strip_prefix("data:")
                .unwrap_or(std::str::from_utf8(line).ok()?.trim())
                .trim();
            (!line.is_empty() && line != "[DONE]")
                .then(|| serde_json::from_str(line).ok())
                .flatten()
        })
        .collect()
}

fn usage_record_from_payload(payload: &Map<String, Value>) -> Option<UsageRecord> {
    let (container, model) = payload.get("response").and_then(Value::as_object).map_or(
        (
            payload,
            payload
                .get("model")
                .and_then(Value::as_str)
                .unwrap_or_default(),
        ),
        |response| {
            (
                response,
                response
                    .get("model")
                    .and_then(Value::as_str)
                    .unwrap_or_default(),
            )
        },
    );
    let usage_map = container.get("usage")?.as_object()?;
    let mut usage = Usage {
        input_tokens: number_field(usage_map, &["input_tokens", "prompt_tokens"]),
        output_tokens: number_field(usage_map, &["output_tokens", "completion_tokens"]),
        total_tokens: number_field(usage_map, &["total_tokens"]),
        ..Usage::default()
    };
    usage.cached_input_tokens = usage_map
        .get("input_tokens_details")
        .and_then(Value::as_object)
        .map_or(0, |details| number_field(details, &["cached_tokens"]));
    usage.reasoning_tokens = usage_map
        .get("output_tokens_details")
        .and_then(Value::as_object)
        .map_or(0, |details| number_field(details, &["reasoning_tokens"]));
    if usage.total_tokens == 0 {
        usage.total_tokens = usage.input_tokens + usage.output_tokens;
    }
    if usage.total_tokens == 0 && usage.input_tokens == 0 && usage.output_tokens == 0 {
        return None;
    }
    usage.requests = 1;
    Some(UsageRecord {
        model: model.into(),
        usage,
        ..UsageRecord::default()
    })
}

fn build_analytics(records: &[UsageRecord]) -> Analytics {
    let mut analytics = Analytics::default();
    let mut by_user = HashMap::new();
    let mut by_account = HashMap::new();
    let mut by_model = HashMap::new();
    let mut first = None;
    let mut last = None;
    for record in records {
        analytics.totals.add(&record.usage);
        add_group(&mut by_user, unknown(&record.user), &record.usage);
        add_group(&mut by_account, unknown(&record.account), &record.usage);
        add_group(&mut by_model, unknown(&record.model), &record.usage);
        if let Some(timestamp) = record.timestamp {
            first = Some(first.map_or(timestamp, |value: DateTime<Utc>| value.min(timestamp)));
            last = Some(last.map_or(timestamp, |value: DateTime<Utc>| value.max(timestamp)));
        }
    }
    analytics.by_user = sorted_groups(by_user);
    analytics.by_account = sorted_groups(by_account);
    analytics.by_model = sorted_groups(by_model);
    analytics.timeline = build_timeline(records, first, last);
    analytics.max_timeline_tokens = analytics
        .timeline
        .iter()
        .map(|bucket| bucket.usage.total_tokens)
        .max()
        .unwrap_or_default();
    analytics.max_user_tokens = analytics
        .by_user
        .iter()
        .map(|group| group.usage.total_tokens)
        .max()
        .unwrap_or_default();
    analytics.max_account_tokens = analytics
        .by_account
        .iter()
        .map(|group| group.usage.total_tokens)
        .max()
        .unwrap_or_default();
    analytics
}

fn add_group(groups: &mut HashMap<String, Usage>, key: &str, usage: &Usage) {
    groups.entry(key.into()).or_default().add(usage);
}

fn sorted_groups(groups: HashMap<String, Usage>) -> Vec<UsageGroup> {
    let mut output: Vec<_> = groups
        .into_iter()
        .map(|(key, usage)| UsageGroup { key, usage })
        .collect();
    output.sort_by(|left, right| {
        right
            .usage
            .total_tokens
            .cmp(&left.usage.total_tokens)
            .then_with(|| left.key.cmp(&right.key))
    });
    output
}

fn build_timeline(
    records: &[UsageRecord],
    first: Option<DateTime<Utc>>,
    last: Option<DateTime<Utc>>,
) -> Vec<UsageBucket> {
    let (Some(first), Some(last)) = (first, last) else {
        return Vec::new();
    };
    let size = bucket_size(last - first);
    let seconds = size.num_seconds();
    let truncate = |value: DateTime<Utc>| {
        Utc.timestamp_opt(value.timestamp().div_euclid(seconds) * seconds, 0)
            .single()
            .unwrap()
    };
    let start = truncate(first);
    let mut usage = BTreeMap::<DateTime<Utc>, Usage>::new();
    for record in records {
        if let Some(timestamp) = record.timestamp {
            usage
                .entry(truncate(timestamp))
                .or_default()
                .add(&record.usage);
        }
    }
    let mut output = Vec::new();
    let mut cursor = start;
    while cursor <= last {
        let label = if size < Duration::hours(1) {
            cursor.format("%H:%M").to_string()
        } else if size < Duration::days(1) {
            cursor.format("%b %d %H:%M").to_string()
        } else {
            cursor.format("%b %d").to_string()
        };
        output.push(UsageBucket {
            start: cursor.to_rfc3339_opts(SecondsFormat::Secs, true),
            label,
            usage: usage.remove(&cursor).unwrap_or_default(),
        });
        cursor += size;
    }
    output
}

fn bucket_size(span: Duration) -> Duration {
    if span <= Duration::hours(2) {
        Duration::minutes(1)
    } else if span <= Duration::hours(48) {
        Duration::hours(1)
    } else {
        Duration::days(1)
    }
}

fn string_field<'a>(values: &'a Map<String, Value>, name: &str) -> Option<&'a str> {
    values.get(name)?.as_str().filter(|value| !value.is_empty())
}

fn bool_field(values: &Map<String, Value>, name: &str) -> bool {
    values
        .get(name)
        .and_then(Value::as_bool)
        .unwrap_or_default()
}

fn number_field(values: &Map<String, Value>, names: &[&str]) -> i64 {
    names
        .iter()
        .find_map(|name| {
            values.get(*name).and_then(|value| {
                value
                    .as_i64()
                    .or_else(|| value.as_f64().map(|value| value as i64))
            })
        })
        .unwrap_or_default()
}

fn unknown(value: &str) -> &str {
    if value.trim().is_empty() {
        "(unknown)"
    } else {
        value
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn records_sanitizes_reassembles_and_analyzes() {
        let temp = tempfile::tempdir().unwrap();
        let recorder = Recorder::new(temp.path()).unwrap();
        recorder.record_meta(
            "codex",
            "session:1",
            Map::from_iter([
                ("user".into(), "alice".into()),
                ("account".into(), "paid".into()),
            ]),
        );
        let body =
            br#"{"model":"gpt-5","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}"#;
        recorder.record_payload_chunk(
            "codex",
            "session:1",
            "response_body",
            "upstream_to_client",
            "stream",
            0,
            0,
            &body[..20],
            Map::new(),
        );
        recorder.record_payload_chunk(
            "codex",
            "session:1",
            "response_body",
            "upstream_to_client",
            "stream",
            1,
            20,
            &body[20..],
            Map::new(),
        );
        recorder.record_payload_summary(
            "codex",
            "session:1",
            "response_body",
            "upstream_to_client",
            "stream",
            body.len() as u64,
            &hex::encode(Sha256::digest(body)),
            2,
            Map::new(),
        );
        let summaries = list_summaries(temp.path()).unwrap();
        assert_eq!(summaries.len(), 1);
        assert!(summaries[0].has_bodies);
        assert_eq!(summaries[0].usage.total_tokens, 13);
        assert!(
            read_sanitized_session(temp.path(), "codex", "session").unwrap()[1]
                .payload
                .get("body_base64")
                .is_none()
        );
        let raw = read_raw_session(temp.path(), "codex", "session").unwrap();
        assert!(raw.last().unwrap().body_text.contains("input_tokens"));
        let analytics = analyze(temp.path()).unwrap();
        assert_eq!(analytics.totals.total_tokens, 13);
        assert_eq!(analytics.by_user[0].key, "alice");
    }

    #[test]
    fn redacts_credential_headers() {
        let headers = HeaderMap::from_iter([
            (
                http::header::AUTHORIZATION,
                "Bearer secret".parse().unwrap(),
            ),
            (
                http::header::CONTENT_TYPE,
                "application/json".parse().unwrap(),
            ),
        ]);
        let redacted = redacted_headers(&headers);
        assert_eq!(redacted["authorization"], ["<redacted>"]);
        assert_eq!(redacted["content-type"], ["application/json"]);
        assert!(!serde_json::to_string(&redacted).unwrap().contains("secret"));
    }
}
