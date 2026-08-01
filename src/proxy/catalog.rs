use std::collections::HashMap;
use std::future::Future;
use std::sync::Arc;

use anyhow::{Context, bail};
use futures_util::future::{BoxFuture, FutureExt, Shared};
use http::{HeaderMap, Method, Uri};
use reqwest::Client;
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use tokio::sync::Mutex;
use url::Url;

const CATALOG_AGGREGATE_MAX_PAGES: usize = 256;
const CATALOG_AGGREGATE_MAX_BYTES: usize = 128 << 20;

#[derive(Clone)]
pub struct BufferedResponse {
    pub status: u16,
    pub headers: HeaderMap,
    pub body: bytes::Bytes,
}

type FlightFuture = Shared<BoxFuture<'static, Arc<BufferedResponse>>>;

/// Shares identical work only while it is in flight. Completed responses are
/// removed immediately, so sequential callers always reach the upstream.
#[derive(Default)]
pub struct SingleFlight {
    calls: Mutex<HashMap<String, (u64, FlightFuture)>>,
    next_id: std::sync::atomic::AtomicU64,
}

impl SingleFlight {
    pub async fn run<F, Fut>(&self, key: String, operation: F) -> Arc<BufferedResponse>
    where
        F: FnOnce() -> Fut + Send + 'static,
        Fut: Future<Output = BufferedResponse> + Send + 'static,
    {
        let (id, future, leader) = {
            let mut calls = self.calls.lock().await;
            if let Some((id, future)) = calls.get(&key) {
                (*id, future.clone(), false)
            } else {
                let id = self
                    .next_id
                    .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                let future = async move { Arc::new(operation().await) }.boxed().shared();
                calls.insert(key.clone(), (id, future.clone()));
                (id, future, true)
            }
        };
        let result = future.await;
        if leader {
            let mut calls = self.calls.lock().await;
            if calls.get(&key).is_some_and(|(current, _)| *current == id) {
                calls.remove(&key);
            }
        }
        result
    }
}

#[must_use]
pub fn coalescable_path(path: &str) -> bool {
    let path = strip_chatgpt_backend_path(path).unwrap_or(path);
    path.starts_with("/ps/plugins/")
        || path == "/plugins/installed"
        || path.starts_with("/plugins/featured")
}

#[must_use]
pub fn flight_key(method: &Method, uri: &Uri, headers: &HeaderMap) -> String {
    let mut digest = Sha256::new();
    let account = header(headers, "chatgpt-account-id");
    let user = header(headers, "chatgpt-user-id");
    if account.is_empty() && user.is_empty() {
        digest.update(b"bearer\0");
        digest.update(header(headers, "authorization"));
    } else {
        digest.update(b"account\0");
        digest.update(account);
        digest.update([0]);
        digest.update(user);
    }
    let identity = hex::encode(&digest.finalize()[..16]);
    format!(
        "{method}\n{}\n{}\n{}\n{identity}",
        uri.path(),
        canonical_query(uri.query().unwrap_or_default()),
        String::from_utf8_lossy(header(headers, "accept-encoding"))
    )
}

fn header<'a>(headers: &'a HeaderMap, name: &str) -> &'a [u8] {
    headers.get(name).map_or(&[], http::HeaderValue::as_bytes)
}

fn canonical_query(query: &str) -> String {
    let mut pairs: Vec<(String, String)> = url::form_urlencoded::parse(query.as_bytes())
        .into_owned()
        .collect();
    pairs.sort();
    url::form_urlencoded::Serializer::new(String::new())
        .extend_pairs(pairs)
        .finish()
}

#[must_use]
pub fn is_catalog_list_path(path: &str) -> bool {
    strip_chatgpt_backend_path(path)
        .unwrap_or(path)
        .starts_with("/ps/plugins/list")
}

#[must_use]
pub fn strip_chatgpt_backend_path(path: &str) -> Option<&str> {
    if path == "/backend-api" {
        Some("/")
    } else {
        path.strip_prefix("/backend-api/")
            .map(|value| &path[path.len() - value.len() - 1..])
    }
}

fn next_page_token(body: &[u8]) -> Option<String> {
    serde_json::from_slice::<Value>(body)
        .ok()?
        .pointer("/pagination/next_page_token")?
        .as_str()
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
}

fn plugins(body: &[u8]) -> Option<Vec<Value>> {
    serde_json::from_slice::<Value>(body)
        .ok()?
        .get("plugins")?
        .as_array()
        .cloned()
}

#[derive(Clone, Eq, PartialEq)]
pub struct AggregateResult {
    pub body: bytes::Bytes,
    pub pages: usize,
    pub entries: usize,
    pub aggregated: bool,
}

#[allow(clippy::too_many_arguments)]
pub async fn aggregate_catalog_pages(
    client: &Client,
    method: &Method,
    original_uri: &Uri,
    upstream_url: &Url,
    headers: &HeaderMap,
    first_body: bytes::Bytes,
) -> anyhow::Result<AggregateResult> {
    if *method != Method::GET || !is_catalog_list_path(original_uri.path()) {
        return Ok(AggregateResult {
            body: first_body,
            pages: 0,
            entries: 0,
            aggregated: false,
        });
    }
    let Some(mut token) = next_page_token(&first_body) else {
        return Ok(AggregateResult {
            body: first_body,
            pages: 0,
            entries: 0,
            aggregated: false,
        });
    };
    let Some(mut merged) = plugins(&first_body) else {
        return Ok(AggregateResult {
            body: first_body,
            pages: 0,
            entries: 0,
            aggregated: false,
        });
    };
    let mut pages = 1;
    let mut byte_count = first_body.len();
    while !token.is_empty()
        && pages < CATALOG_AGGREGATE_MAX_PAGES
        && byte_count < CATALOG_AGGREGATE_MAX_BYTES
    {
        let mut next_url = upstream_url.clone();
        {
            let mut query: Vec<(String, String)> = original_uri
                .query()
                .map(|query| {
                    url::form_urlencoded::parse(query.as_bytes())
                        .into_owned()
                        .collect()
                })
                .unwrap_or_default();
            query.retain(|(key, _)| key != "pageToken");
            query.push(("pageToken".into(), token.clone()));
            next_url.set_query(Some(
                &url::form_urlencoded::Serializer::new(String::new())
                    .extend_pairs(query)
                    .finish(),
            ));
        }
        let mut request = client.get(next_url);
        for (name, value) in headers {
            if !is_hop_by_hop(name.as_str()) && name != http::header::HOST {
                request = request.header(name, value);
            }
        }
        let response = request
            .send()
            .await
            .with_context(|| format!("catalog page {}", pages + 1))?;
        let status = response.status();
        if !status.is_success() {
            bail!(
                "catalog page {}: upstream status {}",
                pages + 1,
                status.as_u16()
            );
        }
        let body = response
            .bytes()
            .await
            .with_context(|| format!("catalog page {} body", pages + 1))?;
        if body.len() > CATALOG_AGGREGATE_MAX_BYTES {
            bail!("catalog page {} exceeds aggregation bounds", pages + 1);
        }
        let page_plugins = plugins(&body)
            .with_context(|| format!("catalog page {}: unparseable body", pages + 1))?;
        merged.extend(page_plugins);
        byte_count += body.len();
        pages += 1;
        token = next_page_token(&body).unwrap_or_default();
    }
    if !token.is_empty() {
        bail!("catalog exceeds aggregation bounds ({pages} pages, {byte_count} bytes)");
    }
    let mut payload: Map<String, Value> =
        serde_json::from_slice(&first_body).context("catalog merge: decode first page")?;
    payload.insert("plugins".into(), Value::Array(merged));
    if let Some(Value::Object(pagination)) = payload.get_mut("pagination") {
        pagination.remove("next_page_token");
    }
    let entries = payload
        .get("plugins")
        .and_then(Value::as_array)
        .map_or(0, Vec::len);
    Ok(AggregateResult {
        body: serde_json::to_vec(&payload)
            .context("catalog merge: encode payload")?
            .into(),
        pages,
        entries,
        aggregated: true,
    })
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

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use axum::Json;
    use axum::extract::{Query, State};
    use axum::routing::get;
    use serde_json::json;

    use super::*;

    #[test]
    fn key_includes_query_encoding_and_identity() {
        let uri: Uri = "/ps/plugins/list?b=2&a=1".parse().unwrap();
        let mut headers = HeaderMap::new();
        headers.insert("chatgpt-account-id", "account-1".parse().unwrap());
        headers.insert("accept-encoding", "gzip".parse().unwrap());
        let first = flight_key(&Method::GET, &uri, &headers);
        let reordered: Uri = "/ps/plugins/list?a=1&b=2".parse().unwrap();
        assert_eq!(first, flight_key(&Method::GET, &reordered, &headers));
        let page_two: Uri = "/ps/plugins/list?a=1&b=2&pageToken=cursor-2"
            .parse()
            .unwrap();
        assert_ne!(first, flight_key(&Method::GET, &page_two, &headers));
        headers.insert("chatgpt-account-id", "account-2".parse().unwrap());
        assert_ne!(first, flight_key(&Method::GET, &uri, &headers));
        assert!(!first.contains("account-1"));
    }

    #[tokio::test]
    async fn concurrent_calls_share_one_operation_but_sequential_calls_do_not() {
        let flight = Arc::new(SingleFlight::default());
        let calls = Arc::new(AtomicUsize::new(0));
        let barrier = Arc::new(tokio::sync::Barrier::new(9));
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let flight = Arc::clone(&flight);
            let calls = Arc::clone(&calls);
            let barrier = Arc::clone(&barrier);
            tasks.push(tokio::spawn(async move {
                barrier.wait().await;
                flight
                    .run("same".into(), move || async move {
                        calls.fetch_add(1, Ordering::SeqCst);
                        tokio::time::sleep(std::time::Duration::from_millis(25)).await;
                        BufferedResponse {
                            status: 200,
                            headers: HeaderMap::new(),
                            body: bytes::Bytes::from_static(b"ok"),
                        }
                    })
                    .await
            }));
        }
        barrier.wait().await;
        for task in tasks {
            assert_eq!(task.await.unwrap().body, "ok");
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        let calls_for_second = Arc::clone(&calls);
        flight
            .run("same".into(), move || async move {
                calls_for_second.fetch_add(1, Ordering::SeqCst);
                BufferedResponse {
                    status: 200,
                    headers: HeaderMap::new(),
                    body: bytes::Bytes::new(),
                }
            })
            .await;
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn completed_call_releases_buffered_response() {
        let flight = SingleFlight::default();
        let response = flight
            .run("release".into(), || async {
                BufferedResponse {
                    status: 200,
                    headers: HeaderMap::new(),
                    body: bytes::Bytes::from(vec![0_u8; 1 << 20]),
                }
            })
            .await;
        let response_weak = Arc::downgrade(&response);
        drop(response);
        tokio::task::yield_now().await;
        assert!(
            response_weak.upgrade().is_none(),
            "completed single-flight response remained retained"
        );
    }

    #[tokio::test]
    async fn catalog_walk_collapses_fourteen_pages_without_a_continuation() {
        let calls = Arc::new(AtomicUsize::new(0));
        let app = axum::Router::new()
            .route("/ps/plugins/list", get(catalog_page))
            .with_state(Arc::clone(&calls));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let uri: Uri = "/ps/plugins/list?limit=170".parse().unwrap();
        let upstream = Url::parse(&format!("http://{address}/ps/plugins/list?limit=170")).unwrap();
        let result = aggregate_catalog_pages(
            &Client::new(),
            &Method::GET,
            &uri,
            &upstream,
            &HeaderMap::new(),
            serde_json::to_vec(&catalog_page_body(0)).unwrap().into(),
        )
        .await
        .unwrap();
        server.abort();

        assert!(result.aggregated);
        assert_eq!(result.pages, 14);
        assert_eq!(result.entries, 2_380);
        assert_eq!(calls.load(Ordering::SeqCst), 13);
        assert!(next_page_token(&result.body).is_none());
        let payload: Value = serde_json::from_slice(&result.body).unwrap();
        assert_eq!(
            payload
                .get("plugins")
                .and_then(Value::as_array)
                .map(Vec::len),
            Some(2_380)
        );
    }

    async fn catalog_page(
        State(calls): State<Arc<AtomicUsize>>,
        Query(query): Query<HashMap<String, String>>,
    ) -> Json<Value> {
        calls.fetch_add(1, Ordering::SeqCst);
        let page = query
            .get("pageToken")
            .and_then(|value| value.parse().ok())
            .unwrap_or_default();
        Json(catalog_page_body(page))
    }

    fn catalog_page_body(page: usize) -> Value {
        const PAGES: usize = 14;
        const PER_PAGE: usize = 170;
        let plugins = (0..PER_PAGE)
            .map(|entry| json!({"id": format!("plugin-{page}-{entry}")}))
            .collect::<Vec<_>>();
        let pagination = if page + 1 < PAGES {
            json!({"next_page_token": (page + 1).to_string()})
        } else {
            json!({})
        };
        json!({"plugins": plugins, "pagination": pagination})
    }
}
