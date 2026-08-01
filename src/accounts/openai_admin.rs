//! OpenAI organization administration and API-key spend reporting.

use std::collections::{BTreeMap, HashMap};
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use chrono::{Datelike as _, TimeZone as _, Utc};
use reqwest::{Client, StatusCode};
use serde::Deserialize;
use serde_json::Value;

use super::{
    AdminKeyEntry, ApiKeyUsageSnapshot, DailyUsage, OpenAiProject, StoredCodexAccount, TopModel,
};

const DEFAULT_OPENAI_API_BASE: &str = "https://api.openai.com";

#[derive(Clone, Copy)]
struct ModelPrice {
    prefix: &'static str,
    input_usd_per_million: f64,
    output_usd_per_million: f64,
    cached_discount: f64,
}

const MODEL_PRICES: &[ModelPrice] = &[
    ModelPrice {
        prefix: "gpt-5.1-codex",
        input_usd_per_million: 1.25,
        output_usd_per_million: 10.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-5.1",
        input_usd_per_million: 1.25,
        output_usd_per_million: 10.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-5-codex",
        input_usd_per_million: 1.25,
        output_usd_per_million: 10.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-5-mini",
        input_usd_per_million: 0.25,
        output_usd_per_million: 2.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-5-nano",
        input_usd_per_million: 0.05,
        output_usd_per_million: 0.40,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-5",
        input_usd_per_million: 1.25,
        output_usd_per_million: 10.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4.1-mini",
        input_usd_per_million: 0.40,
        output_usd_per_million: 1.60,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4.1-nano",
        input_usd_per_million: 0.10,
        output_usd_per_million: 0.40,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4.1",
        input_usd_per_million: 2.0,
        output_usd_per_million: 8.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4o-mini",
        input_usd_per_million: 0.15,
        output_usd_per_million: 0.60,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4o",
        input_usd_per_million: 2.50,
        output_usd_per_million: 10.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4-turbo",
        input_usd_per_million: 10.0,
        output_usd_per_million: 30.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "gpt-4",
        input_usd_per_million: 30.0,
        output_usd_per_million: 60.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "o1-mini",
        input_usd_per_million: 3.0,
        output_usd_per_million: 12.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "o1",
        input_usd_per_million: 15.0,
        output_usd_per_million: 60.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "o3-mini",
        input_usd_per_million: 1.10,
        output_usd_per_million: 4.40,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "o3",
        input_usd_per_million: 10.0,
        output_usd_per_million: 40.0,
        cached_discount: 0.1,
    },
    ModelPrice {
        prefix: "o4-mini",
        input_usd_per_million: 1.10,
        output_usd_per_million: 4.40,
        cached_discount: 0.1,
    },
];

#[derive(Clone, Debug, Default, PartialEq)]
pub struct UsageSummary {
    pub today_usd: f64,
    pub today_cost_estimated: bool,
    pub week_usd: f64,
    pub month_usd: f64,
    pub today_tokens: i64,
    pub week_tokens: i64,
    pub month_tokens: i64,
}

#[derive(Debug)]
struct AdminError {
    status: Option<StatusCode>,
    message: String,
}

impl std::fmt::Display for AdminError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl std::error::Error for AdminError {}

pub async fn validate_admin_key(client: &Client, admin_key: &str) -> anyhow::Result<()> {
    if !admin_key.starts_with("sk-admin-") {
        bail!("invalid admin key format, expected sk-admin-...");
    }
    let projects = fetch_admin_json(
        client,
        admin_key,
        "/v1/organization/projects",
        &[("limit", "1".to_owned())],
    )
    .await;
    match projects {
        Ok(_) => Ok(()),
        Err(error)
            if error
                .downcast_ref::<AdminError>()
                .and_then(|error| error.status)
                == Some(StatusCode::FORBIDDEN) =>
        {
            let start = (Utc::now().timestamp() - 86_400).to_string();
            fetch_admin_json(
                client,
                admin_key,
                "/v1/organization/costs",
                &[
                    ("start_time", start),
                    ("bucket_width", "1d".into()),
                    ("limit", "1".into()),
                ],
            )
            .await
            .map(|_| ())
        }
        Err(error) => Err(error),
    }
}

pub async fn list_openai_projects(
    client: &Client,
    admin_key: &str,
) -> anyhow::Result<Vec<OpenAiProject>> {
    #[derive(Deserialize)]
    struct Page {
        #[serde(default)]
        data: Vec<OpenAiProject>,
        #[serde(default)]
        has_more: bool,
        #[serde(default)]
        last_id: String,
    }

    let mut output = Vec::new();
    let mut after = String::new();
    for _ in 0..20 {
        let mut query = vec![("limit", "100".to_owned())];
        if !after.is_empty() {
            query.push(("after", after.clone()));
        }
        let value =
            fetch_admin_json(client, admin_key, "/v1/organization/projects", &query).await?;
        let page: Page = serde_json::from_value(value).context("decode OpenAI projects")?;
        output.extend(page.data);
        if !page.has_more || page.last_id.is_empty() {
            break;
        }
        after = page.last_id;
    }
    Ok(output)
}

pub async fn fetch_api_key_usage_snapshot(
    client: &Client,
    admin: &AdminKeyEntry,
    account: &StoredCodexAccount,
    days: u32,
) -> anyhow::Result<ApiKeyUsageSnapshot> {
    let (daily, top_model) =
        fetch_openai_usage_rollup(client, &admin.key, days, &account.project_id).await?;
    let summary = summarize_daily_usage(&daily);
    Ok(ApiKeyUsageSnapshot {
        admin_key_label: admin.label.clone(),
        org_id: admin.org_id.clone(),
        project_id: account.project_id.clone(),
        project_name: account.project_name.clone(),
        fetched_at: Utc::now().to_rfc3339(),
        today_usd: summary.today_usd,
        today_cost_estimated: summary.today_cost_estimated,
        week_usd: summary.week_usd,
        month_usd: summary.month_usd,
        today_tokens: summary.today_tokens,
        week_tokens: summary.week_tokens,
        month_tokens: summary.month_tokens,
        top_model,
        daily,
    })
}

pub async fn fetch_openai_usage_rollup(
    client: &Client,
    admin_key: &str,
    days: u32,
    project_id: &str,
) -> anyhow::Result<(Vec<DailyUsage>, Option<TopModel>)> {
    let days = days.clamp(1, 30);
    let start = (Utc::now().timestamp() - i64::from(days) * 86_400).to_string();
    let limit = days.to_string();
    let mut costs_query = vec![
        ("start_time", start.clone()),
        ("bucket_width", "1d".into()),
        ("limit", limit.clone()),
    ];
    let mut usage_query = vec![
        ("start_time", start),
        ("bucket_width", "1d".into()),
        ("limit", limit),
        ("group_by[]", "model".into()),
    ];
    if !project_id.is_empty() {
        costs_query.push(("project_ids[]", project_id.to_owned()));
        usage_query.push(("project_ids[]", project_id.to_owned()));
    }
    let (costs, usage, today) = tokio::try_join!(
        fetch_admin_json(client, admin_key, "/v1/organization/costs", &costs_query),
        fetch_admin_json(
            client,
            admin_key,
            "/v1/organization/usage/completions",
            &usage_query
        ),
        fetch_today_hourly(client, admin_key, project_id),
    )?;
    combine_usage(costs, usage, today)
}

pub fn summarize_daily_usage(daily: &[DailyUsage]) -> UsageSummary {
    let today = Utc::now().date_naive().to_string();
    let week_start = daily.len().saturating_sub(7);
    let month_start = daily.len().saturating_sub(30);
    let mut output = UsageSummary::default();
    for (index, row) in daily.iter().enumerate() {
        let tokens = row.input_tokens + row.cached_input_tokens + row.output_tokens;
        if row.date == today {
            output.today_usd = row.cost_usd;
            output.today_cost_estimated = row.cost_estimated;
            output.today_tokens = tokens;
        }
        if index >= week_start {
            output.week_usd += row.cost_usd;
            output.week_tokens += tokens;
        }
        if index >= month_start {
            output.month_usd += row.cost_usd;
            output.month_tokens += tokens;
        }
    }
    output
}

#[must_use]
pub fn estimate_cost_usd(
    model: &str,
    input_tokens: i64,
    cached_input_tokens: i64,
    output_tokens: i64,
) -> Option<f64> {
    let model = model.to_ascii_lowercase();
    let price = MODEL_PRICES
        .iter()
        .find(|price| model.starts_with(price.prefix))?;
    Some(
        input_tokens as f64 / 1_000_000.0 * price.input_usd_per_million
            + cached_input_tokens as f64 / 1_000_000.0
                * price.input_usd_per_million
                * price.cached_discount
            + output_tokens as f64 / 1_000_000.0 * price.output_usd_per_million,
    )
}

#[derive(Default)]
struct TodayHourly {
    date: String,
    input_tokens: i64,
    cached_input_tokens: i64,
    output_tokens: i64,
    requests: i64,
    estimated_cost_usd: f64,
    tokens_by_model: HashMap<String, i64>,
}

async fn fetch_today_hourly(
    client: &Client,
    admin_key: &str,
    project_id: &str,
) -> anyhow::Result<Option<TodayHourly>> {
    let now = Utc::now();
    let midnight = Utc
        .with_ymd_and_hms(now.year(), now.month(), now.day(), 0, 0, 0)
        .single()
        .ok_or_else(|| anyhow!("invalid UTC date"))?;
    let mut query = vec![
        ("start_time", midnight.timestamp().to_string()),
        ("bucket_width", "1h".into()),
        ("limit", "24".into()),
        ("group_by[]", "model".into()),
    ];
    if !project_id.is_empty() {
        query.push(("project_ids[]", project_id.to_owned()));
    }
    let value = fetch_admin_json(
        client,
        admin_key,
        "/v1/organization/usage/completions",
        &query,
    )
    .await?;
    let mut output = TodayHourly {
        date: midnight.date_naive().to_string(),
        ..TodayHourly::default()
    };
    for result in result_values(&value) {
        let input = integer(result, "input_tokens");
        let cached = integer(result, "input_cached_tokens");
        let output_tokens = integer(result, "output_tokens");
        let requests = integer(result, "num_model_requests");
        let model = string(result, "model").unwrap_or("(unknown)").to_owned();
        output.input_tokens += input;
        output.cached_input_tokens += cached;
        output.output_tokens += output_tokens;
        output.requests += requests;
        *output.tokens_by_model.entry(model.clone()).or_default() += input + cached + output_tokens;
        if let Some(cost) = estimate_cost_usd(&model, input, cached, output_tokens) {
            output.estimated_cost_usd += cost;
        }
    }
    if output.requests == 0 && output.input_tokens == 0 && output.output_tokens == 0 {
        Ok(None)
    } else {
        Ok(Some(output))
    }
}

fn combine_usage(
    costs: Value,
    usage: Value,
    today: Option<TodayHourly>,
) -> anyhow::Result<(Vec<DailyUsage>, Option<TopModel>)> {
    let mut by_date: BTreeMap<String, DailyUsage> = BTreeMap::new();
    for bucket in costs
        .get("data")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let date = bucket_date(bucket)?;
        let row = by_date.entry(date.clone()).or_insert_with(|| DailyUsage {
            date,
            ..DailyUsage::default()
        });
        for result in bucket
            .get("results")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            row.cost_usd += flexible_f64(result.pointer("/amount/value"));
        }
    }
    let mut tokens_by_model: HashMap<String, i64> = HashMap::new();
    for bucket in usage
        .get("data")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let date = bucket_date(bucket)?;
        let row = by_date.entry(date.clone()).or_insert_with(|| DailyUsage {
            date,
            ..DailyUsage::default()
        });
        for result in bucket
            .get("results")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let input = integer(result, "input_tokens");
            let cached = integer(result, "input_cached_tokens");
            let output = integer(result, "output_tokens");
            let model = string(result, "model").unwrap_or("(unknown)").to_owned();
            row.input_tokens += input;
            row.cached_input_tokens += cached;
            row.output_tokens += output;
            row.requests += integer(result, "num_model_requests");
            *tokens_by_model.entry(model).or_default() += input + cached + output;
        }
    }
    if let Some(today) = today {
        let row = by_date
            .entry(today.date.clone())
            .or_insert_with(|| DailyUsage {
                date: today.date,
                ..DailyUsage::default()
            });
        row.input_tokens = today.input_tokens;
        row.cached_input_tokens = today.cached_input_tokens;
        row.output_tokens = today.output_tokens;
        row.requests = today.requests;
        if row.cost_usd == 0.0 && today.estimated_cost_usd > 0.0 {
            row.cost_usd = today.estimated_cost_usd;
            row.cost_estimated = true;
        }
        for (model, tokens) in today.tokens_by_model {
            *tokens_by_model.entry(model).or_default() += tokens;
        }
    }
    let top_model = tokens_by_model
        .into_iter()
        .max_by(|left, right| left.1.cmp(&right.1).then_with(|| right.0.cmp(&left.0)))
        .map(|(model, tokens)| TopModel { model, tokens });
    Ok((by_date.into_values().collect(), top_model))
}

fn result_values(value: &Value) -> impl Iterator<Item = &Value> {
    value
        .get("data")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .flat_map(|bucket| {
            bucket
                .get("results")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
        })
}

fn bucket_date(bucket: &Value) -> anyhow::Result<String> {
    if let Some(iso) = string(bucket, "start_time_iso").filter(|iso| iso.len() >= 10) {
        return Ok(iso[..10].to_owned());
    }
    let timestamp = integer(bucket, "start_time");
    Utc.timestamp_opt(timestamp, 0)
        .single()
        .map(|value| value.date_naive().to_string())
        .ok_or_else(|| anyhow!("OpenAI usage bucket has an invalid start_time"))
}

fn string<'a>(value: &'a Value, field: &str) -> Option<&'a str> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
}

fn integer(value: &Value, field: &str) -> i64 {
    value.get(field).and_then(Value::as_i64).unwrap_or_default()
}

fn flexible_f64(value: Option<&Value>) -> f64 {
    value
        .and_then(|value| {
            value
                .as_f64()
                .or_else(|| value.as_str().and_then(|value| value.parse().ok()))
        })
        .unwrap_or_default()
}

async fn fetch_admin_json(
    client: &Client,
    admin_key: &str,
    path: &str,
    query: &[(&str, String)],
) -> anyhow::Result<Value> {
    let base = std::env::var("SUBROUTER_OPENAI_API_BASE")
        .ok()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_OPENAI_API_BASE.to_owned());
    let url = format!("{}{}", base.trim_end_matches('/'), path);
    let mut last_error = None;
    for attempt in 0..4 {
        match client
            .get(&url)
            .query(query)
            .bearer_auth(admin_key)
            .header(reqwest::header::ACCEPT, "application/json")
            .send()
            .await
        {
            Ok(response) => {
                let status = response.status();
                let bytes = response
                    .bytes()
                    .await
                    .context("read OpenAI admin response")?;
                if status.is_success() {
                    return serde_json::from_slice(&bytes).context("decode OpenAI admin response");
                }
                let mut detail = String::from_utf8_lossy(&bytes).trim().to_owned();
                detail.truncate(200);
                let error = AdminError {
                    status: Some(status),
                    message: format!("OpenAI {status} {path}: {detail}"),
                };
                if !status.is_server_error() {
                    return Err(error.into());
                }
                last_error = Some(anyhow!(error));
            }
            Err(error) => last_error = Some(error.into()),
        }
        if attempt < 3 {
            tokio::time::sleep(Duration::from_secs(1 << attempt)).await;
        }
    }
    Err(last_error.unwrap_or_else(|| anyhow!("OpenAI admin request failed")))
}

#[cfg(test)]
mod tests {
    use chrono::Days;

    use super::*;

    #[test]
    fn combines_flexible_costs_and_usage() {
        let costs = serde_json::json!({"data":[{"start_time_iso":"2026-07-31T00:00:00Z","results":[{"amount":{"value":"1.25"}}]}]});
        let usage = serde_json::json!({"data":[{"start_time_iso":"2026-07-31T00:00:00Z","results":[{"input_tokens":100,"input_cached_tokens":20,"output_tokens":5,"num_model_requests":2,"model":"gpt-5"}]}]});
        let (daily, top) = combine_usage(costs, usage, None).unwrap();
        assert_eq!(daily.len(), 1);
        assert_eq!(daily[0].cost_usd, 1.25);
        assert_eq!(daily[0].requests, 2);
        assert_eq!(
            top.unwrap(),
            TopModel {
                model: "gpt-5".into(),
                tokens: 125
            }
        );
    }

    #[test]
    fn pricing_prefers_longest_prefix() {
        let value = estimate_cost_usd("gpt-5-mini-2026", 1_000_000, 0, 1_000_000).unwrap();
        assert!((value - 2.25).abs() < f64::EPSILON);
    }

    #[test]
    fn summary_uses_trailing_windows() {
        let daily = (0..10)
            .map(|offset| DailyUsage {
                date: Utc::now()
                    .date_naive()
                    .checked_sub_days(Days::new(9 - offset))
                    .unwrap()
                    .to_string(),
                cost_usd: 1.0,
                input_tokens: 1,
                ..DailyUsage::default()
            })
            .collect::<Vec<_>>();
        let summary = summarize_daily_usage(&daily);
        assert_eq!(summary.today_usd, 1.0);
        assert_eq!(summary.week_usd, 7.0);
        assert_eq!(summary.month_usd, 10.0);
    }
}
