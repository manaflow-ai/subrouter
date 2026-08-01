use std::net::SocketAddr;

use axum::Router;
use axum::body::{Body, to_bytes};
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{DefaultBodyLimit, FromRequestParts, Request};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{any, get};
use clap::Parser;
use futures_util::StreamExt;
use tracing::{error, info};
use tracing_subscriber::EnvFilter;

#[derive(Parser)]
struct Options {
    #[arg(long, default_value = "127.0.0.1:8799")]
    addr: SocketAddr,
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("mockupstream: {error:#}");
        std::process::exit(1);
    }
}

async fn run() -> anyhow::Result<()> {
    let _ = tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .with_target(false)
        .try_init();
    let options = Options::parse();
    let app = Router::new()
        .route("/v1/responses", any(responses))
        .route("/v1/models", get(models))
        .fallback(not_found)
        .layer(DefaultBodyLimit::max(1 << 20));
    let listener = tokio::net::TcpListener::bind(options.addr).await?;
    info!(addr = %options.addr, "mock upstream listening");
    axum::serve(listener, app).await?;
    Ok(())
}

async fn responses(request: Request) -> Response {
    let (mut parts, body) = request.into_parts();
    let upgrade = WebSocketUpgrade::from_request_parts(&mut parts, &())
        .await
        .ok();
    let request = Request::from_parts(parts, body);
    if let Some(upgrade) = upgrade {
        let headers = request.headers().clone();
        return upgrade
            .on_failed_upgrade(|error| error!(%error, "WebSocket upgrade failed"))
            .on_upgrade(move |socket| responses_websocket(socket, headers))
            .into_response();
    }
    if request.method() != axum::http::Method::POST {
        return (StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n").into_response();
    }
    let headers = request.headers().clone();
    let body = to_bytes(request.into_body(), 1 << 20)
        .await
        .unwrap_or_default();
    info!(
        auth = %redact_bearer(header(&headers, "authorization")),
        account_id = header(&headers, "chatgpt-account-id"),
        window = header(&headers, "x-codex-window-id"),
        turn_state = header(&headers, "x-codex-turn-state"),
        bytes = body.len(),
        "responses request"
    );
    let mut response = Response::new(Body::from(sse_events()));
    response
        .headers_mut()
        .insert("content-type", "text/event-stream".parse().unwrap());
    response
        .headers_mut()
        .insert("cache-control", "no-cache".parse().unwrap());
    response
        .headers_mut()
        .insert("x-codex-turn-state", "mock-turn-state".parse().unwrap());
    response
}

async fn responses_websocket(mut socket: WebSocket, headers: HeaderMap) {
    info!(
        auth = %redact_bearer(header(&headers, "authorization")),
        account_id = header(&headers, "chatgpt-account-id"),
        window = header(&headers, "x-codex-window-id"),
        turn_state = header(&headers, "x-codex-turn-state"),
        "responses WebSocket"
    );
    while let Some(message) = socket.next().await {
        let Ok(message) = message else { return };
        if !matches!(message, Message::Text(_)) {
            continue;
        }
        for event in response_events() {
            if socket.send(Message::Text(event.into())).await.is_err() {
                return;
            }
        }
    }
}

async fn models() -> impl IntoResponse {
    (
        [("content-type", "application/json")],
        r#"{"object":"list","data":[{"id":"mock-model","object":"model","created":0,"owned_by":"subrouter"}]}"#,
    )
}

async fn not_found() -> impl IntoResponse {
    (StatusCode::NOT_FOUND, "not found\n")
}

fn response_events() -> [&'static str; 3] {
    [
        r#"{"type":"response.created","response":{"id":"resp_subrouter_smoke"}}"#,
        r#"{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_subrouter_smoke","content":[{"type":"output_text","text":"subrouter smoke ok"}]}}"#,
        r#"{"type":"response.completed","response":{"id":"resp_subrouter_smoke","usage":{"input_tokens":0,"input_tokens_details":null,"output_tokens":0,"output_tokens_details":null,"total_tokens":0}}}"#,
    ]
}

fn sse_events() -> String {
    response_events()
        .into_iter()
        .map(|event| {
            let kind = serde_json::from_str::<serde_json::Value>(event)
                .ok()
                .and_then(|value| {
                    value
                        .get("type")
                        .and_then(serde_json::Value::as_str)
                        .map(str::to_owned)
                })
                .unwrap_or_else(|| "message".into());
            format!("event: {kind}\ndata: {event}\n\n")
        })
        .collect()
}

fn header<'a>(headers: &'a HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
}

fn redact_bearer(value: &str) -> String {
    let Some(token) = value.strip_prefix("Bearer ") else {
        return String::new();
    };
    if token.len() <= 10 {
        "Bearer ***".into()
    } else {
        format!("Bearer {}...{}", &token[..6], &token[token.len() - 4..])
    }
}
