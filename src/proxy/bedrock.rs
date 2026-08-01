use std::collections::{BTreeMap, HashMap};
use std::fs::{self, OpenOptions};
use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime};

use anyhow::{anyhow, bail};
use aws_credential_types::provider::{ProvideCredentials, SharedCredentialsProvider};
use aws_sdk_servicequotas::Client as ServiceQuotasClient;
use aws_sigv4::http_request::{SignableBody, SignableRequest, SigningSettings, sign};
use aws_sigv4::sign::v4;
use axum::body::{Body, to_bytes};
use axum::response::{IntoResponse, Response};
use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64};
use bytes::Bytes;
use chrono::{DateTime, Local, Utc};
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, StatusCode};
use reqwest::{Client, Url};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use subtle::ConstantTimeEq;
use tracing::{error, warn};

const SERVICE: &str = "bedrock";
const FABLE_MODEL_ID: &str = "us.anthropic.claude-fable-5";
const MAX_BODY_BYTES: usize = 128 << 20;
const MAX_FRAME_BYTES: usize = 64 << 20;

#[derive(Clone)]
pub struct BedrockCredentialSource {
    pub name: String,
    pub credentials: SharedCredentialsProvider,
    pub bumper: Option<Arc<BedrockQuotaBumper>>,
}

impl std::fmt::Debug for BedrockCredentialSource {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BedrockCredentialSource")
            .field("name", &self.name)
            .field("quota_bumper_configured", &self.bumper.is_some())
            .finish_non_exhaustive()
    }
}

pub struct BedrockConfig {
    regions: Vec<String>,
    sources: Vec<BedrockCredentialSource>,
    gateway_token: String,
    cost_log_path: PathBuf,
    client: Client,
    next_attempt: AtomicU64,
    cost_lock: Mutex<()>,
    endpoint_override: Option<Url>,
}

impl std::fmt::Debug for BedrockConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BedrockConfig")
            .field("regions", &self.regions)
            .field(
                "sources",
                &self
                    .sources
                    .iter()
                    .map(|source| source.name.as_str())
                    .collect::<Vec<_>>(),
            )
            .field(
                "gateway_token",
                &if self.gateway_token.is_empty() {
                    "<empty>"
                } else {
                    "<redacted>"
                },
            )
            .field("cost_log_path", &self.cost_log_path)
            .finish_non_exhaustive()
    }
}

impl BedrockConfig {
    pub fn new(
        regions: Vec<String>,
        sources: Vec<BedrockCredentialSource>,
        gateway_token: impl Into<String>,
        cost_log_path: impl Into<PathBuf>,
    ) -> anyhow::Result<Self> {
        let client = Client::builder()
            .connect_timeout(Duration::from_secs(30))
            .pool_idle_timeout(Duration::from_secs(10))
            .pool_max_idle_per_host(64)
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        Ok(Self {
            regions: normalize_regions(regions),
            sources: normalize_sources(sources),
            gateway_token: gateway_token.into().trim().into(),
            cost_log_path: cost_log_path.into(),
            client,
            next_attempt: AtomicU64::new(0),
            cost_lock: Mutex::new(()),
            endpoint_override: None,
        })
    }

    #[cfg(test)]
    fn with_endpoint(mut self, endpoint: Url) -> Self {
        self.endpoint_override = Some(endpoint);
        self
    }

    #[must_use]
    pub fn configured(&self) -> bool {
        !self.regions.is_empty() && !self.sources.is_empty()
    }

    #[must_use]
    pub fn regions(&self) -> &[String] {
        &self.regions
    }

    #[must_use]
    pub fn source_names(&self) -> Vec<&str> {
        self.sources
            .iter()
            .map(|source| source.name.as_str())
            .collect()
    }

    pub async fn handle_gateway(&self, request: Request<Body>) -> Response {
        if !self.configured() {
            return text(
                StatusCode::SERVICE_UNAVAILABLE,
                "bedrock gateway not configured\n",
            );
        }
        if !self.gateway_token.is_empty()
            && !gateway_token_ok(request.headers(), &self.gateway_token)
        {
            return text(StatusCode::UNAUTHORIZED, "unauthorized\n");
        }
        let method = request.method().clone();
        let path = request
            .uri()
            .path()
            .trim_start_matches("/bedrock")
            .to_owned();
        if path.is_empty() || path == "/" {
            return text(StatusCode::NOT_FOUND, "404 page not found\n");
        }
        let query = request.uri().query().unwrap_or_default().to_owned();
        let headers = filtered_request_headers(request.headers());
        let body = match to_bytes(request.into_body(), MAX_BODY_BYTES).await {
            Ok(value) => value,
            Err(_) => return text(StatusCode::BAD_REQUEST, "failed to read request body\n"),
        };
        let started = Instant::now();
        let response = match self.forward(&method, &path, &query, headers, &body).await {
            Ok(value) => value,
            Err(error) => {
                error!(path, %error, "Bedrock upstream request failed");
                return text(StatusCode::BAD_GATEWAY, "bedrock upstream request failed\n");
            }
        };
        self.record_cost(
            &path,
            &response.region,
            response.status,
            started.elapsed(),
            usage_from_response(&path, &response.body),
        );
        response.into_axum()
    }

    pub async fn fable_response(&self, body: &[u8]) -> anyhow::Result<ForwardResponse> {
        let mut payload: Map<String, Value> = serde_json::from_slice(body)?;
        let stream = payload
            .get("stream")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        payload.remove("model");
        payload.remove("stream");
        payload.remove("context_management");
        payload.insert("anthropic_version".into(), "bedrock-2023-05-31".into());
        let body = serde_json::to_vec(&payload)?;
        let endpoint = if stream {
            "invoke-with-response-stream"
        } else {
            "invoke"
        };
        let path = format!("/model/{FABLE_MODEL_ID}/{endpoint}");
        let started = Instant::now();
        let mut response = self
            .forward(
                &Method::POST,
                &path,
                "",
                HeaderMap::from_iter([(
                    http::header::CONTENT_TYPE,
                    HeaderValue::from_static("application/json"),
                )]),
                &body,
            )
            .await?;
        let usage = if stream && response.status.is_success() {
            let transcoded = transcode_event_stream(&response.body);
            if transcoded.events == 0 || transcoded.first_exception {
                response.status = StatusCode::SERVICE_UNAVAILABLE;
                response.headers = HeaderMap::from_iter([(
                    http::header::CONTENT_TYPE,
                    HeaderValue::from_static("application/json"),
                )]);
                response.body = if transcoded.error_payload.is_empty() {
                    Bytes::from_static(br#"{"type":"error","error":{"type":"api_error","message":"Bedrock stream failed before first event"}}"#)
                } else {
                    transcoded.error_payload
                };
                transcoded.usage
            } else {
                response.headers = HeaderMap::from_iter([
                    (
                        http::header::CONTENT_TYPE,
                        HeaderValue::from_static("text/event-stream"),
                    ),
                    (
                        http::header::CACHE_CONTROL,
                        HeaderValue::from_static("no-cache"),
                    ),
                ]);
                response.body = transcoded.body;
                transcoded.usage
            }
        } else {
            parse_invoke_usage(&response.body)
        };
        self.record_cost(
            &path,
            &response.region,
            response.status,
            started.elapsed(),
            usage,
        );
        Ok(response)
    }

    async fn forward(
        &self,
        method: &Method,
        path: &str,
        query: &str,
        headers: HeaderMap,
        body: &[u8],
    ) -> anyhow::Result<ForwardResponse> {
        let attempts = self.ordered_attempts();
        if attempts.is_empty() {
            bail!("no Bedrock region and credential source pairs are configured");
        }
        let mut first_error = None;
        for (index, (region, source)) in attempts.iter().enumerate() {
            match self
                .forward_once(source, region, method, path, query, &headers, body)
                .await
            {
                Ok(response) => {
                    if (response.status == StatusCode::TOO_MANY_REQUESTS
                        || response.status.is_server_error())
                        && index + 1 < attempts.len()
                    {
                        if response.status == StatusCode::TOO_MANY_REQUESTS {
                            self.on_throttle(source, region, model_from_path(path));
                        }
                        warn!(
                            bedrock_source = %source.name,
                            region,
                            status = response.status.as_u16(),
                            "Bedrock source unusable, retrying next source"
                        );
                        continue;
                    }
                    if response.status == StatusCode::TOO_MANY_REQUESTS {
                        self.on_throttle(source, region, model_from_path(path));
                    }
                    return Ok(response);
                }
                Err(error) => {
                    warn!(bedrock_source = %source.name, region, path, %error, "Bedrock source failed");
                    first_error.get_or_insert(error);
                }
            }
        }
        Err(first_error.unwrap_or_else(|| anyhow!("all Bedrock attempts failed")))
    }

    #[allow(clippy::too_many_arguments)]
    async fn forward_once(
        &self,
        source: &BedrockCredentialSource,
        region: &str,
        method: &Method,
        path: &str,
        query: &str,
        headers: &HeaderMap,
        body: &[u8],
    ) -> anyhow::Result<ForwardResponse> {
        let mut target = self.endpoint(region)?;
        target.set_path(path);
        target.set_query((!query.is_empty()).then_some(query));
        let credentials = source.credentials.provide_credentials().await?;
        let signed_headers = signed_headers(method, &target, headers, body, credentials, region)?;
        let mut request = self.client.request(method.clone(), target);
        for (name, value) in &signed_headers {
            request = request.header(name, value);
        }
        let response = request.body(body.to_vec()).send().await?;
        let status = response.status();
        let headers = response.headers().clone();
        let body = response.bytes().await?;
        Ok(ForwardResponse {
            status,
            headers,
            body,
            source: source.name.clone(),
            region: region.into(),
        })
    }

    fn endpoint(&self, region: &str) -> anyhow::Result<Url> {
        if let Some(endpoint) = &self.endpoint_override {
            return Ok(endpoint.clone());
        }
        Ok(Url::parse(&format!(
            "https://bedrock-runtime.{region}.amazonaws.com"
        ))?)
    }

    fn ordered_attempts(&self) -> Vec<(String, BedrockCredentialSource)> {
        let mut attempts = Vec::with_capacity(self.regions.len() * self.sources.len());
        for region in &self.regions {
            for source in &self.sources {
                attempts.push((region.clone(), source.clone()));
            }
        }
        if attempts.len() > 1 {
            let offset =
                self.next_attempt.fetch_add(1, Ordering::Relaxed) as usize % attempts.len();
            attempts.rotate_left(offset);
        }
        attempts
    }

    fn on_throttle(&self, source: &BedrockCredentialSource, region: &str, model: &str) {
        let Some(bumper) = &source.bumper else { return };
        bumper.spawn(region.to_owned(), model.to_owned());
    }

    fn record_cost(
        &self,
        path: &str,
        region: &str,
        status: StatusCode,
        elapsed: Duration,
        usage: Option<Usage>,
    ) {
        if self.cost_log_path.as_os_str().is_empty() {
            return;
        }
        let model = model_from_path(path);
        if model.is_empty() {
            return;
        }
        let usage = usage.unwrap_or_default();
        let record = CostRecord {
            timestamp: Utc::now(),
            model: model.into(),
            region: region.into(),
            status: status.as_u16(),
            usage,
            cost_usd_estimate: usage.cost_usd(model),
            duration_ms: i64::try_from(elapsed.as_millis()).unwrap_or(i64::MAX),
        };
        let Ok(mut line) = serde_json::to_vec(&record) else {
            return;
        };
        line.push(b'\n');
        let _guard = self
            .cost_lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(parent) = self.cost_log_path.parent()
            && fs::create_dir_all(parent).is_err()
        {
            return;
        }
        let mut options = OpenOptions::new();
        options.create(true).append(true).write(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt as _;
            options.mode(0o600);
        }
        if let Ok(mut file) = options.open(&self.cost_log_path) {
            let _ = file.write_all(&line);
        }
    }

    #[must_use]
    pub fn cost_summary(&self) -> CostSummary {
        summarize_cost(&self.cost_log_path)
    }
}

#[derive(Clone)]
pub struct ForwardResponse {
    pub status: StatusCode,
    pub headers: HeaderMap,
    pub body: Bytes,
    pub source: String,
    pub region: String,
}

impl ForwardResponse {
    pub fn into_axum(self) -> Response {
        let mut builder = Response::builder().status(self.status);
        let destination = builder.headers_mut().expect("response builder headers");
        for (name, value) in &self.headers {
            if !hop_by_hop(name.as_str()) && *name != http::header::CONTENT_LENGTH {
                destination.append(name, value.clone());
            }
        }
        builder
            .body(Body::from(self.body))
            .unwrap_or_else(|_| text(StatusCode::INTERNAL_SERVER_ERROR, "response build failed\n"))
    }
}

fn signed_headers(
    method: &Method,
    target: &Url,
    headers: &HeaderMap,
    body: &[u8],
    credentials: aws_credential_types::Credentials,
    region: &str,
) -> anyhow::Result<HeaderMap> {
    let mut output = headers.clone();
    if !output.contains_key(http::header::CONTENT_TYPE) {
        output.insert(
            http::header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
    }
    let owned = output
        .iter()
        .filter_map(|(name, value)| {
            value
                .to_str()
                .ok()
                .map(|value| (name.as_str().to_owned(), value.to_owned()))
        })
        .collect::<Vec<_>>();
    let identity = credentials.into();
    let params = v4::SigningParams::builder()
        .identity(&identity)
        .region(region)
        .name(SERVICE)
        .time(SystemTime::now())
        .settings(SigningSettings::default())
        .build()?
        .into();
    let signable = SignableRequest::new(
        method.as_str(),
        target.as_str(),
        owned
            .iter()
            .map(|(name, value)| (name.as_str(), value.as_str())),
        SignableBody::Bytes(body),
    )?;
    let (instructions, _) = sign(signable, &params)?.into_parts();
    for (name, value) in instructions.headers() {
        output.insert(
            HeaderName::from_bytes(name.as_bytes())?,
            HeaderValue::from_str(value)?,
        );
    }
    Ok(output)
}

fn normalize_regions(regions: Vec<String>) -> Vec<String> {
    let mut output = Vec::new();
    for region in regions {
        let region = region.trim();
        if !region.is_empty() && !output.iter().any(|value| value == region) {
            output.push(region.into());
        }
    }
    output
}

fn normalize_sources(sources: Vec<BedrockCredentialSource>) -> Vec<BedrockCredentialSource> {
    sources
        .into_iter()
        .map(|mut source| {
            source.name = source.name.trim().into();
            if source.name.is_empty() {
                source.name = "default".into();
            }
            source
        })
        .collect()
}

fn filtered_request_headers(source: &HeaderMap) -> HeaderMap {
    let mut output = HeaderMap::new();
    for (name, value) in source {
        let lower = name.as_str().to_ascii_lowercase();
        if hop_by_hop(&lower)
            || matches!(lower.as_str(), "authorization" | "host" | "content-length")
            || lower.starts_with("x-amz-")
        {
            continue;
        }
        output.append(name, value.clone());
    }
    if !output.contains_key(http::header::CONTENT_TYPE) {
        output.insert(
            http::header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
    }
    output
}

fn gateway_token_ok(headers: &HeaderMap, expected: &str) -> bool {
    let raw = headers
        .get(http::header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .trim();
    let presented = raw
        .split_once(' ')
        .filter(|(scheme, _)| scheme.eq_ignore_ascii_case("bearer"))
        .map_or(raw, |(_, token)| token.trim());
    presented.len() == expected.len() && bool::from(presented.as_bytes().ct_eq(expected.as_bytes()))
}

fn hop_by_hop(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "connection"
            | "proxy-connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
    )
}

fn model_from_path(path: &str) -> &str {
    path.strip_prefix("/model/")
        .and_then(|rest| rest.split('/').next())
        .unwrap_or_default()
}

fn usage_from_response(path: &str, body: &[u8]) -> Option<Usage> {
    if path.ends_with("/invoke-with-response-stream") {
        event_stream_usage(body)
    } else {
        parse_invoke_usage(body)
    }
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Usage {
    pub input_tokens: i64,
    pub output_tokens: i64,
    #[serde(rename = "cache_read_input_tokens")]
    pub cache_read_tokens: i64,
    #[serde(rename = "cache_creation_input_tokens")]
    pub cache_write_tokens: i64,
}

impl Usage {
    fn empty(self) -> bool {
        self.input_tokens == 0
            && self.output_tokens == 0
            && self.cache_read_tokens == 0
            && self.cache_write_tokens == 0
    }

    fn cost_usd(self, model: &str) -> f64 {
        let model = model.to_ascii_lowercase();
        let (input, output, cache_read, cache_write) = if model.contains("fable") {
            (10.0, 50.0, 1.0, 12.5)
        } else if model.contains("opus") {
            (5.0, 25.0, 0.5, 6.25)
        } else if model.contains("sonnet") {
            (3.0, 15.0, 0.3, 3.75)
        } else if model.contains("haiku") {
            (1.0, 5.0, 0.1, 1.25)
        } else {
            (0.0, 0.0, 0.0, 0.0)
        };
        (self.input_tokens as f64 * input
            + self.output_tokens as f64 * output
            + self.cache_read_tokens as f64 * cache_read
            + self.cache_write_tokens as f64 * cache_write)
            / 1_000_000.0
    }
}

fn parse_invoke_usage(body: &[u8]) -> Option<Usage> {
    let value: Value = serde_json::from_slice(body).ok()?;
    let usage: Usage = serde_json::from_value(value.get("usage")?.clone()).ok()?;
    (!usage.empty()).then_some(usage)
}

fn event_stream_usage(body: &[u8]) -> Option<Usage> {
    let mut usage = Usage::default();
    for frame in event_stream_frames(body) {
        let Some(payload) = frame.decoded_payload() else {
            continue;
        };
        absorb_usage(&mut usage, &payload);
    }
    (!usage.empty()).then_some(usage)
}

fn absorb_usage(usage: &mut Usage, payload: &[u8]) {
    let Ok(value) = serde_json::from_slice::<Value>(payload) else {
        return;
    };
    match value.get("type").and_then(Value::as_str) {
        Some("message_start") => {
            if let Ok(found) = serde_json::from_value::<Usage>(
                value.pointer("/message/usage").cloned().unwrap_or_default(),
            ) {
                usage.input_tokens = found.input_tokens;
                usage.cache_read_tokens = found.cache_read_tokens;
                usage.cache_write_tokens = found.cache_write_tokens;
            }
        }
        Some("message_delta") => {
            if let Ok(found) =
                serde_json::from_value::<Usage>(value.get("usage").cloned().unwrap_or_default())
            {
                usage.output_tokens = usage.output_tokens.max(found.output_tokens);
            }
        }
        _ => {}
    }
}

struct Transcoded {
    body: Bytes,
    usage: Option<Usage>,
    events: usize,
    first_exception: bool,
    error_payload: Bytes,
}

fn transcode_event_stream(body: &[u8]) -> Transcoded {
    let frames = event_stream_frames(body);
    let mut output = Vec::new();
    let mut usage = Usage::default();
    let mut events = 0usize;
    let mut saw_stop = false;
    let mut first_exception = false;
    let mut error_payload = Bytes::new();
    for frame in frames {
        if frame.message_type.eq_ignore_ascii_case("exception") {
            first_exception = events == 0;
            error_payload = frame.payload.clone();
            let error_type = if frame
                .exception_type
                .to_ascii_lowercase()
                .contains("throttl")
            {
                "overloaded_error"
            } else {
                "api_error"
            };
            let message = if error_type == "overloaded_error" {
                "Bedrock throttled mid-stream"
            } else {
                "Bedrock stream error"
            };
            let payload = serde_json::to_vec(
                &json!({"type":"error","error":{"type":error_type,"message":message}}),
            )
            .unwrap_or_default();
            output.extend_from_slice(b"event: error\ndata: ");
            output.extend_from_slice(&payload);
            output.extend_from_slice(b"\n\n");
            break;
        }
        let Some(payload) = frame.decoded_payload() else {
            continue;
        };
        let event_type = serde_json::from_slice::<Value>(&payload)
            .ok()
            .and_then(|value| value.get("type").and_then(Value::as_str).map(str::to_owned))
            .unwrap_or_else(|| "message".into());
        absorb_usage(&mut usage, &payload);
        saw_stop |= event_type == "message_stop";
        output.extend_from_slice(format!("event: {event_type}\ndata: ").as_bytes());
        output.extend_from_slice(&payload);
        output.extend_from_slice(b"\n\n");
        events += 1;
    }
    if events > 0 && !saw_stop && !first_exception {
        output.extend_from_slice(
            br#"event: error
data: {"type":"error","error":{"type":"api_error","message":"Bedrock stream interrupted"}}

"#,
        );
    }
    Transcoded {
        body: output.into(),
        usage: (!usage.empty()).then_some(usage),
        events,
        first_exception,
        error_payload,
    }
}

#[derive(Default)]
struct EventFrame {
    payload: Bytes,
    message_type: String,
    exception_type: String,
}

impl EventFrame {
    fn decoded_payload(&self) -> Option<Vec<u8>> {
        if self.message_type.eq_ignore_ascii_case("exception") {
            return Some(self.payload.to_vec());
        }
        let value: Value = serde_json::from_slice(&self.payload).ok()?;
        BASE64.decode(value.get("bytes")?.as_str()?).ok()
    }
}

fn event_stream_frames(mut body: &[u8]) -> Vec<EventFrame> {
    let mut output = Vec::new();
    while body.len() >= 16 {
        let total = u32::from_be_bytes(body[0..4].try_into().expect("four bytes")) as usize;
        let headers_len = u32::from_be_bytes(body[4..8].try_into().expect("four bytes")) as usize;
        if !(16..=MAX_FRAME_BYTES).contains(&total) || total > body.len() {
            break;
        }
        let payload_end = total - 4;
        let header_end = 12usize.saturating_add(headers_len);
        let (headers, payload_start) = if header_end <= payload_end {
            (
                parse_event_headers(&body[12..header_end]).unwrap_or_default(),
                header_end,
            )
        } else {
            (HashMap::new(), 12)
        };
        output.push(EventFrame {
            payload: Bytes::copy_from_slice(&body[payload_start..payload_end]),
            message_type: headers.get(":message-type").cloned().unwrap_or_default(),
            exception_type: headers.get(":exception-type").cloned().unwrap_or_default(),
        });
        body = &body[total..];
    }
    output
}

fn parse_event_headers(mut input: &[u8]) -> Option<HashMap<String, String>> {
    let mut output = HashMap::new();
    while !input.is_empty() {
        let name_len = *input.first()? as usize;
        input = input.get(1..)?;
        let name = std::str::from_utf8(input.get(..name_len)?).ok()?.to_owned();
        input = input.get(name_len..)?;
        let kind = *input.first()?;
        input = input.get(1..)?;
        match kind {
            0 | 1 => {}
            2 => input = input.get(1..)?,
            3 => input = input.get(2..)?,
            4 => input = input.get(4..)?,
            5 | 8 => input = input.get(8..)?,
            6 | 7 => {
                let len = u16::from_be_bytes(input.get(..2)?.try_into().ok()?) as usize;
                input = input.get(2..)?;
                let value = input.get(..len)?;
                if kind == 7 {
                    output.insert(name, std::str::from_utf8(value).ok()?.to_owned());
                }
                input = input.get(len..)?;
            }
            9 => input = input.get(16..)?,
            _ => return None,
        }
    }
    Some(output)
}

#[derive(Clone, Debug, Deserialize, Serialize)]
struct CostRecord {
    timestamp: DateTime<Utc>,
    model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    region: String,
    status: u16,
    usage: Usage,
    cost_usd_estimate: f64,
    duration_ms: i64,
}

#[derive(Clone, Copy, Debug, Default, Serialize)]
pub struct ModelCost {
    pub requests: u64,
    pub total_usd: f64,
    pub input_tokens: i64,
    pub output_tokens: i64,
}

#[derive(Clone, Debug, Default, Serialize)]
pub struct CostSummary {
    pub requests: u64,
    pub total_usd: f64,
    pub today_usd: f64,
    pub week_7d_usd: f64,
    pub month_30d_usd: f64,
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    pub throttled: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub last_throttle: String,
    pub by_model: BTreeMap<String, ModelCost>,
}

fn summarize_cost(path: &Path) -> CostSummary {
    let Ok(file) = fs::File::open(path) else {
        return CostSummary::default();
    };
    let now = Utc::now();
    let local_today = Local::now().date_naive();
    let mut output = CostSummary::default();
    for line in BufReader::new(file).lines().map_while(Result::ok) {
        let Ok(record) = serde_json::from_str::<CostRecord>(&line) else {
            continue;
        };
        output.requests += 1;
        output.total_usd += record.cost_usd_estimate;
        output.input_tokens += record.usage.input_tokens;
        output.output_tokens += record.usage.output_tokens;
        output.cache_read_tokens += record.usage.cache_read_tokens;
        output.cache_write_tokens += record.usage.cache_write_tokens;
        if record.status == StatusCode::TOO_MANY_REQUESTS.as_u16() {
            output.throttled += 1;
            let stamp = record.timestamp.to_rfc3339();
            if stamp > output.last_throttle {
                output.last_throttle = stamp;
            }
        }
        if record.timestamp.with_timezone(&Local).date_naive() == local_today {
            output.today_usd += record.cost_usd_estimate;
        }
        if record.timestamp > now - chrono::Duration::days(7) {
            output.week_7d_usd += record.cost_usd_estimate;
        }
        if record.timestamp > now - chrono::Duration::days(30) {
            output.month_30d_usd += record.cost_usd_estimate;
        }
        let model = record
            .model
            .rsplit('.')
            .next()
            .unwrap_or(&record.model)
            .to_owned();
        let aggregate = output.by_model.entry(model).or_default();
        aggregate.requests += 1;
        aggregate.total_usd += record.cost_usd_estimate;
        aggregate.input_tokens += record.usage.input_tokens;
        aggregate.output_tokens += record.usage.output_tokens;
    }
    output
}

pub struct BedrockQuotaBumper {
    sdk: aws_config::SdkConfig,
    clients: Mutex<HashMap<String, ServiceQuotasClient>>,
    last: Mutex<HashMap<String, Instant>>,
    cooldown: Duration,
    max_value: f64,
}

impl std::fmt::Debug for BedrockQuotaBumper {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BedrockQuotaBumper")
            .field("cooldown", &self.cooldown)
            .field("max_value", &self.max_value)
            .finish_non_exhaustive()
    }
}

impl BedrockQuotaBumper {
    #[must_use]
    pub fn new(sdk: aws_config::SdkConfig) -> Arc<Self> {
        Arc::new(Self {
            sdk,
            clients: Mutex::new(HashMap::new()),
            last: Mutex::new(HashMap::new()),
            cooldown: Duration::from_secs(6 * 60 * 60),
            max_value: 20_000_000.0,
        })
    }

    fn spawn(self: &Arc<Self>, region: String, model: String) {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            this.bump(&region, &model).await;
        });
    }

    async fn bump(&self, region: &str, model: &str) {
        let code = if model.to_ascii_lowercase().contains("fable") {
            "L-9B258944"
        } else if model.to_ascii_lowercase().contains("opus") {
            "L-DB99DCDB"
        } else {
            return;
        };
        let key = format!("{region}\0{code}");
        {
            let mut last = self
                .last
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if last
                .get(&key)
                .is_some_and(|instant| instant.elapsed() < self.cooldown)
            {
                return;
            }
            last.insert(key, Instant::now());
        }
        let client = {
            let mut clients = self
                .clients
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            clients
                .entry(region.into())
                .or_insert_with(|| ServiceQuotasClient::new(&self.sdk))
                .clone()
        };
        let current = tokio::time::timeout(
            Duration::from_secs(20),
            client
                .get_service_quota()
                .service_code(SERVICE)
                .quota_code(code)
                .send(),
        )
        .await;
        let Ok(Ok(current)) = current else {
            warn!(
                region,
                model,
                quota = code,
                "Bedrock autobump could not read quota"
            );
            return;
        };
        let Some(value) = current.quota().and_then(|quota| quota.value()) else {
            return;
        };
        let desired = (value * 2.0).min(self.max_value);
        if desired <= value {
            return;
        }
        match tokio::time::timeout(
            Duration::from_secs(20),
            client
                .request_service_quota_increase()
                .service_code(SERVICE)
                .quota_code(code)
                .desired_value(desired)
                .send(),
        )
        .await
        {
            Ok(Ok(_)) => warn!(
                region,
                model,
                quota = code,
                current = value,
                desired,
                "Bedrock autobump requested quota increase"
            ),
            _ => warn!(
                region,
                model,
                quota = code,
                current = value,
                desired,
                "Bedrock autobump request failed"
            ),
        }
    }
}

fn text(status: StatusCode, value: &str) -> Response {
    (status, value.to_owned()).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use aws_credential_types::Credentials;

    fn credentials(access_key: &str) -> SharedCredentialsProvider {
        SharedCredentialsProvider::new(Credentials::new(access_key, "secret", None, None, "test"))
    }

    fn frame(payload: &str, headers: &[(&str, &str)]) -> Vec<u8> {
        let payload = serde_json::to_vec(&json!({"bytes": BASE64.encode(payload)})).unwrap();
        let mut header_block = Vec::new();
        for (name, value) in headers {
            header_block.push(name.len() as u8);
            header_block.extend_from_slice(name.as_bytes());
            header_block.push(7);
            header_block.extend_from_slice(&(value.len() as u16).to_be_bytes());
            header_block.extend_from_slice(value.as_bytes());
        }
        let total = 16 + header_block.len() + payload.len();
        let mut output = vec![0; total];
        output[0..4].copy_from_slice(&(total as u32).to_be_bytes());
        output[4..8].copy_from_slice(&(header_block.len() as u32).to_be_bytes());
        output[12..12 + header_block.len()].copy_from_slice(&header_block);
        output[12 + header_block.len()..total - 4].copy_from_slice(&payload);
        output
    }

    #[test]
    fn signs_with_bedrock_scope_without_exposing_secret() {
        let target =
            Url::parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke").unwrap();
        let headers = signed_headers(
            &Method::POST,
            &target,
            &HeaderMap::new(),
            b"{}",
            Credentials::new("AKIDEXAMPLE", "not-a-real-secret", None, None, "test"),
            "us-east-1",
        )
        .unwrap();
        let authorization = headers
            .get(http::header::AUTHORIZATION)
            .unwrap()
            .to_str()
            .unwrap();
        assert!(authorization.contains("Credential=AKIDEXAMPLE/"));
        assert!(authorization.contains("/us-east-1/bedrock/aws4_request"));
        assert!(!authorization.contains("not-a-real-secret"));
    }

    #[test]
    fn transcodes_event_stream_and_extracts_usage() {
        let mut body = frame(
            r#"{"type":"message_start","message":{"usage":{"input_tokens":10}}}"#,
            &[(":message-type", "event")],
        );
        body.extend(frame(
            r#"{"type":"message_delta","usage":{"output_tokens":7}}"#,
            &[(":message-type", "event")],
        ));
        body.extend(frame(
            r#"{"type":"message_stop"}"#,
            &[(":message-type", "event")],
        ));
        let result = transcode_event_stream(&body);
        let text = String::from_utf8(result.body.to_vec()).unwrap();
        assert!(text.contains("event: message_start"));
        assert!(text.contains("event: message_stop"));
        assert_eq!(result.usage.unwrap().input_tokens, 10);
        assert_eq!(result.usage.unwrap().output_tokens, 7);
    }

    #[test]
    fn config_rotates_region_source_pairs() {
        let sources = vec![
            BedrockCredentialSource {
                name: "aw0".into(),
                credentials: credentials("A"),
                bumper: None,
            },
            BedrockCredentialSource {
                name: "aw1".into(),
                credentials: credentials("B"),
                bumper: None,
            },
        ];
        let config =
            BedrockConfig::new(vec!["east".into(), "west".into()], sources, "", "").unwrap();
        let first = config.ordered_attempts();
        let second = config.ordered_attempts();
        assert_eq!(
            (first[0].0.as_str(), first[0].1.name.as_str()),
            ("east", "aw0")
        );
        assert_eq!(
            (second[0].0.as_str(), second[0].1.name.as_str()),
            ("east", "aw1")
        );
    }

    #[tokio::test]
    async fn gateway_rejects_missing_token_before_forwarding() {
        let source = BedrockCredentialSource {
            name: "test".into(),
            credentials: credentials("A"),
            bumper: None,
        };
        let config = BedrockConfig::new(vec!["us-east-1".into()], vec![source], "secret", "")
            .unwrap()
            .with_endpoint(Url::parse("http://127.0.0.1:1").unwrap());
        let request = Request::post("/bedrock/model/x/invoke")
            .body(Body::from("{}"))
            .unwrap();
        assert_eq!(
            config.handle_gateway(request).await.status(),
            StatusCode::UNAUTHORIZED
        );
    }
}
