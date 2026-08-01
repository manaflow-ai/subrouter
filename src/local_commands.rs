//! Local account workflows that sit above the credential stores.

use std::env;
use std::io::{self, BufRead as _, IsTerminal as _, Write as _};
use std::process::Stdio;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use chrono::{DateTime, Utc};
use reqwest::Client;
use serde::Deserialize;
use serde_json::Value;
use tokio::process::Command;

use crate::account::{Account, AuthMode};
use crate::accounts::{
    self, AdminKeyEntry, ApiKeyUsageSnapshot, CodexStore, CodexUsageDetails, StoredCodexAccount,
    UsageWindow,
};
use crate::agents::claude;
use crate::broker::{self, CredentialSource};
use crate::selectacct::{LimitWindow, Scheduler, Score, score_from_limit_windows};
use crate::servers::{self, ServerConfig};

const CLAUDE_ROUTING_ENV_KEYS: &[&str] = &[
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_API_KEY",
    "CLAUDE_CODE_USE_BEDROCK",
    "ANTHROPIC_BEDROCK_BASE_URL",
    "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
    "CLAUDE_CODE_USE_VERTEX",
    "ANTHROPIC_VERTEX_BASE_URL",
    "CLAUDE_CODE_SKIP_VERTEX_AUTH",
    "CLAUDE_CODE_USE_MANTLE",
    "ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
    "CLAUDE_CODE_SKIP_MANTLE_AUTH",
    "CLAUDE_CODE_USE_ANTHROPIC_AWS",
    "ANTHROPIC_AWS_BASE_URL",
    "CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
];

pub async fn maybe_dispatch(args: &[String]) -> Option<anyhow::Result<()>> {
    if let Ok(Some(server)) = selected_legacy_server()
        && let Some(result) = remote_dispatch(&server, args).await
    {
        return Some(result);
    }
    let command = args.first().map(String::as_str);
    let result = match command {
        None => interactive_switch(false).await,
        Some("switch" | "use") if args.len() == 1 => interactive_switch(false).await,
        Some("g" | "gui" | "gui-switch" | "gui-use") => gui_switch(&args[1..]).await,
        Some("pick") => pick_best(false).await,
        Some("reset") => reset(&args[1..]).await,
        Some("usage") => usage(&args[1..]).await,
        Some("add-admin-key") => add_admin_key(&args[1..]).await,
        Some("admin-keys" | "list-admin-keys") => list_admin_keys(),
        Some("remove-admin-key") => remove_admin_key(&args[1..]),
        Some("attach-project") => attach_project(&args[1..]).await,
        Some("claude-aws") => claude_aws(&args[1..]).await,
        Some("claude-direct") => claude_direct(&args[1..]).await,
        Some("spend" | "cost") => spend().await,
        Some(selector) if selector.contains('@') => status_one(selector).await,
        _ => return None,
    };
    Some(result)
}

fn selected_legacy_server() -> anyhow::Result<Option<ServerConfig>> {
    let config = broker::load_config(None)?;
    if config.effective_credential_source() != CredentialSource::Legacy {
        return Ok(None);
    }
    servers::Store::default().select(None)
}

struct UsageRow {
    stored: StoredCodexAccount,
    account: Account,
    details: Option<CodexUsageDetails>,
    score: Score,
    error: Option<String>,
}

async fn usage_rows() -> anyhow::Result<Vec<UsageRow>> {
    let store = CodexStore::default();
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let mut rows = Vec::new();
    for stored in store.list_stored()? {
        if stored.is_api_key() {
            let Some(account) = stored.to_account(stored.source_path(&store)) else {
                continue;
            };
            rows.push(UsageRow {
                score: Score {
                    account_id: account.id.clone(),
                    provider: account.provider,
                    headroom: 0.01,
                    short_headroom: 0.01,
                    fresh: true,
                    ..Score::default()
                },
                stored,
                account,
                details: None,
                error: None,
            });
            continue;
        }
        let original = stored.clone();
        let (stored, refresh_error) = match store
            .refresh_stored_if_expired(&client, stored, "sr.status")
            .await
        {
            Ok((stored, _)) => (stored, None),
            Err(error) => {
                let Some(account) = original.to_account(original.source_path(&store)) else {
                    continue;
                };
                rows.push(UsageRow {
                    score: Score {
                        account_id: account.id.clone(),
                        provider: account.provider,
                        ..Score::default()
                    },
                    stored: original,
                    account,
                    details: None,
                    error: Some(error.to_string()),
                });
                continue;
            }
        };
        let Some(account) = stored.to_account(stored.source_path(&store)) else {
            continue;
        };
        match accounts::fetch_codex_usage_details(&client, &account).await {
            Ok(details) => {
                let mut score = score_for(&account, &details.windows);
                score.fresh = true;
                rows.push(UsageRow {
                    stored,
                    account,
                    details: Some(details),
                    score,
                    error: refresh_error,
                });
            }
            Err(error) => rows.push(UsageRow {
                score: Score {
                    account_id: account.id.clone(),
                    provider: account.provider,
                    ..Score::default()
                },
                stored,
                account,
                details: None,
                error: Some(error.to_string()),
            }),
        }
    }
    rows.sort_by(|left, right| {
        let left_tier = row_tier(left);
        let right_tier = row_tier(right);
        left_tier
            .cmp(&right_tier)
            .then_with(|| right.score.headroom.total_cmp(&left.score.headroom))
            .then_with(|| left.account.id.cmp(&right.account.id))
    });
    Ok(rows)
}

fn score_for(account: &Account, windows: &[UsageWindow]) -> Score {
    let limits = windows
        .iter()
        .map(|window| LimitWindow {
            name: window.name.clone(),
            used_percent: window.used_percent,
            limit_window_seconds: window.limit_window_seconds,
            reset_after_seconds: window.reset_after_seconds,
            feature: window.feature.clone(),
        })
        .collect::<Vec<_>>();
    let mut score = score_from_limit_windows(&account.id, 0, &limits);
    score.provider = account.provider;
    score
}

fn row_tier(row: &UsageRow) -> u8 {
    match (
        row.error.is_some(),
        row.account.auth_mode,
        row.score.usable_for_new_session(),
        row.score.exhausted(),
    ) {
        (true, _, _, _) => 4,
        (false, AuthMode::Oauth, true, _) => 0,
        (false, AuthMode::ApiKey, _, _) => 1,
        (false, AuthMode::Oauth, false, false) => 2,
        (false, AuthMode::Oauth, false, true) => 3,
    }
}

async fn status_one(selector: &str) -> anyhow::Result<()> {
    let rows = usage_rows().await?;
    let selector = selector.to_ascii_lowercase();
    let matches = rows
        .into_iter()
        .filter(|row| row.account.id.to_ascii_lowercase().contains(&selector))
        .collect::<Vec<_>>();
    if matches.is_empty() {
        bail!("no account found for {selector}");
    }
    print_usage_rows(&matches, false);
    Ok(())
}

async fn pick_best(restart_gui: bool) -> anyhow::Result<()> {
    let rows = usage_rows().await?;
    if rows.is_empty() {
        bail!("no accounts configured. Run 'sr add' to add one");
    }
    let scheduler = Scheduler::new(rows.iter().map(|row| row.score.clone()));
    let accounts = rows
        .iter()
        .filter(|row| row.error.is_none())
        .map(|row| row.account.clone())
        .collect::<Vec<_>>();
    let target = scheduler.pick(&accounts)?;
    let score = scheduler.score_for(target.provider, &target.id);
    if target.auth_mode == AuthMode::Oauth && !score.usable_for_new_session() {
        print_usage_rows(&rows, false);
        bail!("no recommended account has quota for a new session");
    }
    let active = CodexStore::default().detect_active_account()?;
    if active.as_deref() == Some(&target.id) {
        println!("Already using recommended account: {}", target.id);
        return Ok(());
    }
    crate::cli::switch_account(&CodexStore::default(), &target.id)?;
    if restart_gui {
        restart_codex_gui().await?;
    }
    println!("Picked recommended account: {}", target.id);
    Ok(())
}

async fn interactive_switch(restart_gui: bool) -> anyhow::Result<()> {
    let rows = usage_rows().await?;
    if rows.is_empty() {
        println!("No accounts configured. Run 'sr add' to add one.");
        return Ok(());
    }
    let active = CodexStore::default().detect_active_account()?;
    print_usage_rows(&rows, true);
    if !io::stdin().is_terminal() {
        return Ok(());
    }
    print!("Switch to (#): ");
    io::stdout().flush()?;
    let mut answer = String::new();
    io::stdin().lock().read_line(&mut answer)?;
    let answer = answer.trim();
    if answer.is_empty() {
        return Ok(());
    }
    let selector = answer
        .parse::<usize>()
        .ok()
        .filter(|index| *index > 0 && *index <= rows.len())
        .map_or(answer, |index| rows[index - 1].account.id.as_str());
    if active.as_deref() != Some(selector) {
        crate::cli::switch_account(&CodexStore::default(), selector)?;
    }
    if restart_gui {
        restart_codex_gui().await?;
    }
    Ok(())
}

async fn gui_switch(args: &[String]) -> anyhow::Result<()> {
    let selector = args.iter().find(|argument| !argument.starts_with('-'));
    match selector {
        Some(selector) => {
            crate::cli::switch_account(&CodexStore::default(), selector)?;
            restart_codex_gui().await
        }
        None => interactive_switch(true).await,
    }
}

async fn restart_codex_gui() -> anyhow::Result<()> {
    if !cfg!(target_os = "macos") {
        println!("Codex.app restart is only supported on macOS.");
        return Ok(());
    }
    let running = Command::new("pgrep")
        .args(["-x", "Codex"])
        .status()
        .await
        .is_ok_and(|status| status.success());
    if !running {
        println!("Codex.app is not running; it will use the new account on next launch.");
        return Ok(());
    }
    let _ = Command::new("pkill")
        .args(["-TERM", "-x", "Codex"])
        .status()
        .await;
    tokio::time::sleep(Duration::from_millis(500)).await;
    let status = Command::new("open")
        .args(["-a", "Codex"])
        .status()
        .await
        .context("restart Codex.app")?;
    if !status.success() {
        bail!("Codex.app restart failed with {status}");
    }
    println!("Restarted Codex.app so the GUI uses the new account.");
    Ok(())
}

fn print_usage_rows(rows: &[UsageRow], numbered: bool) {
    let active = CodexStore::default().detect_active_account().ok().flatten();
    for (index, row) in rows.iter().enumerate() {
        let prefix = if numbered {
            format!("{}) ", index + 1)
        } else {
            String::new()
        };
        let marker = if active.as_deref() == Some(&row.account.id) {
            " (active)"
        } else {
            ""
        };
        if let Some(error) = &row.error {
            println!("{prefix}{}{marker}\terror: {error}", row.account.id);
            continue;
        }
        if row.account.auth_mode == AuthMode::ApiKey {
            let spend = CodexStore::default()
                .pick_admin_key_for(&row.stored)
                .ok()
                .flatten()
                .and_then(|admin| {
                    CodexStore::default()
                        .read_usage_cache_stale(&admin.label, &row.stored.project_id)
                        .ok()
                        .flatten()
                })
                .map_or_else(
                    || "API key".into(),
                    |snapshot| format!("API key, 30d ${:.2}", snapshot.month_usd),
                );
            println!("{prefix}{}{marker}\t{spend}", row.account.id);
            continue;
        }
        let limits = row
            .details
            .as_ref()
            .map(|details| {
                details
                    .windows
                    .iter()
                    .map(|window| {
                        let reset = if window.reset_after_seconds > 0 {
                            {
                                format!(
                                    " resets in {}",
                                    humantime::format_duration(Duration::from_secs(
                                        window.reset_after_seconds as u64
                                    ))
                                )
                            }
                        } else {
                            Default::default()
                        };
                        format!("{} {:.0}% used{reset}", window.name, window.used_percent)
                    })
                    .collect::<Vec<_>>()
                    .join(", ")
            })
            .unwrap_or_else(|| "available".into());
        println!("{prefix}{}{marker}\t{limits}", row.account.id);
    }
}

#[derive(Default)]
struct ResetOptions {
    email: String,
    all: bool,
    gto: bool,
    count: usize,
    list: bool,
    dry_run: bool,
    help: bool,
}

fn parse_reset_options(args: &[String]) -> anyhow::Result<ResetOptions> {
    let mut options = ResetOptions {
        count: 1,
        ..ResetOptions::default()
    };
    let mut index = 0;
    while index < args.len() {
        match args[index].as_str() {
            "--all" => options.all = true,
            "--gto" => options.gto = true,
            "--list" => options.list = true,
            "--dry-run" => options.dry_run = true,
            "-n" | "--count" => {
                index += 1;
                options.count = args
                    .get(index)
                    .ok_or_else(|| anyhow!("-n requires a value"))?
                    .parse()
                    .context("-n must be an integer")?;
            }
            value if value.starts_with("-n=") || value.starts_with("--count=") => {
                options.count = value
                    .split_once('=')
                    .map(|(_, value)| value)
                    .unwrap_or_default()
                    .parse()
                    .context("-n must be an integer")?;
            }
            "help" | "-h" | "--help" => {
                options.help = true;
                return Ok(options);
            }
            value if value.starts_with('-') => bail!("unknown reset option {value:?}"),
            value if options.email.is_empty() => options.email = value.into(),
            value => bail!("unexpected reset argument {value:?}"),
        }
        index += 1;
    }
    if !options.email.is_empty() && options.all {
        bail!("pass either an email or --all, not both");
    }
    if options.list && (!options.email.is_empty() || options.all || options.gto) {
        bail!("--list only reports credits; do not combine it with an email, --all, or --gto");
    }
    if options.gto && (!options.email.is_empty() || options.all) {
        bail!("--gto selects candidates itself; do not combine it with an email or --all");
    }
    if !options.gto && options.count != 1 {
        bail!("-n only applies with --gto");
    }
    if options.count == 0 {
        bail!("-n must be at least 1");
    }
    Ok(options)
}

struct ResetCandidate {
    account: Account,
    downtime: i64,
    weekly_headroom: f64,
    weekly_exhausted: bool,
}

async fn reset(args: &[String]) -> anyhow::Result<()> {
    let options = parse_reset_options(args)?;
    if options.help {
        println!("usage: sr reset [email] [--all] [--gto [-n N]] [--list] [--dry-run]");
        return Ok(());
    }
    let store = CodexStore::default();
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    if options.list {
        return list_reset_credits(&store, &client).await;
    }
    let mut candidates = Vec::new();
    let mut usable = 0;
    for stored in store
        .list_stored()?
        .into_iter()
        .filter(|account| !account.is_api_key())
    {
        if !options.email.is_empty() && !account_selector_matches(&stored, &options.email) {
            continue;
        }
        let (stored, _) = store
            .refresh_stored_if_expired(&client, stored, "sr.reset")
            .await?;
        let Some(account) = stored.to_account(stored.source_path(&store)) else {
            continue;
        };
        let details = accounts::fetch_codex_usage_details(&client, &account).await?;
        let cooked = details
            .windows
            .iter()
            .any(|window| window.used_percent >= 100.0);
        let credit = details
            .complimentary_reset
            .as_ref()
            .is_some_and(|reset| reset.available);
        if !cooked {
            usable += 1;
        }
        if !cooked || !credit {
            if !options.email.is_empty() {
                bail!(
                    "{} is not eligible for a reset (cooked={cooked}, credit={credit})",
                    account.email
                );
            }
            continue;
        }
        let downtime = details
            .windows
            .iter()
            .filter(|window| window.used_percent >= 100.0)
            .map(|window| window.reset_after_seconds)
            .max()
            .unwrap_or_default();
        let weekly = details
            .windows
            .iter()
            .filter(|window| window.limit_window_seconds > 6 * 60 * 60)
            .collect::<Vec<_>>();
        let weekly_headroom = weekly
            .iter()
            .map(|window| 1.0 - window.used_percent.clamp(0.0, 100.0) / 100.0)
            .reduce(f64::min)
            .unwrap_or(1.0);
        let weekly_exhausted = weekly.iter().any(|window| window.used_percent >= 100.0);
        candidates.push(ResetCandidate {
            account,
            downtime,
            weekly_headroom,
            weekly_exhausted,
        });
    }
    if !options.email.is_empty() && candidates.is_empty() {
        bail!("account {} not found or not eligible", options.email);
    }
    if options.gto {
        candidates.sort_by(|left, right| {
            left.weekly_exhausted
                .cmp(&right.weekly_exhausted)
                .then_with(|| right.weekly_headroom.total_cmp(&left.weekly_headroom))
                .then_with(|| right.downtime.cmp(&left.downtime))
                .then_with(|| left.account.email.cmp(&right.account.email))
        });
        println!("{}", reset_value_verdict(usable, &candidates));
        candidates.truncate(options.count);
    } else if !options.all && options.email.is_empty() {
        candidates.sort_by(|left, right| {
            right
                .downtime
                .cmp(&left.downtime)
                .then_with(|| left.account.email.cmp(&right.account.email))
        });
        candidates.truncate(1);
    }
    if candidates.is_empty() {
        if options.dry_run {
            println!("No accounts are eligible for a rate-limit reset.");
            return Ok(());
        }
        bail!("no cooked account has a rate-limit reset credit available");
    }
    let total = candidates.len();
    let mut reset_count = 0;
    for candidate in candidates {
        if options.dry_run {
            println!(
                "  {}: eligible, {:.0}% weekly headroom after reset, saves {}",
                candidate.account.email,
                candidate.weekly_headroom * 100.0,
                humantime::format_duration(Duration::from_secs(candidate.downtime.max(0) as u64))
            );
            continue;
        }
        let credit = accounts::redeem_rate_limit_reset(&client, &candidate.account).await?;
        reset_count += 1;
        println!(
            "  {}: reset (credit {})",
            candidate.account.email, credit.status
        );
    }
    println!(
        "{} {reset_count}/{total} accounts.",
        if options.dry_run {
            "Would reset"
        } else {
            "Reset"
        }
    );
    Ok(())
}

fn reset_value_verdict(usable: usize, candidates: &[ResetCandidate]) -> String {
    if usable > 0 {
        return format!(
            "LOW VALUE: {usable} Codex account(s) already usable; no reset is needed to unblock."
        );
    }
    let soonest = candidates
        .iter()
        .map(|candidate| candidate.downtime)
        .filter(|seconds| *seconds > 0)
        .min()
        .unwrap_or_default();
    if soonest > 0 && soonest < 20 * 60 {
        return format!(
            "LOW VALUE: every cooked account self-heals within {}; a reset saves little.",
            humantime::format_duration(Duration::from_secs(soonest as u64))
        );
    }
    if candidates.is_empty() {
        return "No cooked Codex account holds a reset credit.".into();
    }
    if soonest == 0 {
        "GOOD VALUE: all Codex accounts are cooked with no near-term natural reset.".into()
    } else {
        format!(
            "GOOD VALUE: all Codex accounts are cooked; soonest natural recovery in {}.",
            humantime::format_duration(Duration::from_secs(soonest as u64))
        )
    }
}

async fn list_reset_credits(store: &CodexStore, client: &Client) -> anyhow::Result<()> {
    let mut total = 0;
    let mut accounts_with_credits = 0;
    for stored in store
        .list_stored()?
        .into_iter()
        .filter(|account| !account.is_api_key())
    {
        let (stored, _) = store
            .refresh_stored_if_expired(client, stored, "sr.reset.list")
            .await?;
        let Some(account) = stored.to_account(stored.source_path(store)) else {
            continue;
        };
        match accounts::list_rate_limit_reset_credits(client, &account).await {
            Ok(credits) => {
                let credits = credits
                    .into_iter()
                    .filter(|credit| credit.status.is_empty() || credit.status == "available")
                    .collect::<Vec<_>>();
                if credits.is_empty() {
                    continue;
                }
                total += credits.len();
                accounts_with_credits += 1;
                let expiry = credits
                    .iter()
                    .filter_map(|credit| DateTime::parse_from_rfc3339(&credit.expires_at).ok())
                    .min()
                    .map(|expiry| {
                        let duration = (expiry.with_timezone(&Utc) - Utc::now())
                            .to_std()
                            .unwrap_or_default();
                        format!(
                            ", soonest expires in {}",
                            humantime::format_duration(duration)
                        )
                    })
                    .unwrap_or_else(|| ", no expiry reported".into());
                println!("  {:28} {} credit(s){expiry}", account.email, credits.len());
            }
            Err(error) => println!("  {:28} error: {error}", account.email),
        }
    }
    println!("{total} reset credit(s) across {accounts_with_credits} account(s).");
    Ok(())
}

fn account_selector_matches(account: &StoredCodexAccount, selector: &str) -> bool {
    let selector = selector.to_ascii_lowercase();
    account.email.to_ascii_lowercase() == selector
        || account.email.to_ascii_lowercase().contains(&selector)
}

async fn add_admin_key(args: &[String]) -> anyhow::Result<()> {
    let label =
        flag_value(args, "--label").map_or_else(|| prompt_line("Label (e.g. work): "), Ok)?;
    let key = flag_value(args, "--key")
        .map_or_else(|| prompt_secret("Admin key (sk-admin-...): "), Ok)?;
    if !key.starts_with("sk-admin-") {
        bail!("invalid admin key format, expected sk-admin-...");
    }
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    print!("Validating with OpenAI... ");
    io::stdout().flush()?;
    accounts::validate_admin_key(&client, &key).await?;
    println!("ok");
    CodexStore::default().save_admin_key(&AdminKeyEntry {
        label: label.clone(),
        key,
        added_at: Utc::now().to_rfc3339(),
        ..AdminKeyEntry::default()
    })?;
    println!("Added admin key: {label}");
    Ok(())
}

fn list_admin_keys() -> anyhow::Result<()> {
    let keys = CodexStore::default().list_admin_keys()?;
    if keys.is_empty() {
        println!("No admin keys. Run 'sr add-admin-key' to add one.");
        return Ok(());
    }
    for key in keys {
        println!(
            "{}\t{}\tadded {}",
            key.label,
            mask_secret(&key.key),
            key.added_at
        );
    }
    Ok(())
}

fn remove_admin_key(args: &[String]) -> anyhow::Result<()> {
    let label = args
        .first()
        .ok_or_else(|| anyhow!("usage: sr remove-admin-key <label>"))?;
    if !CodexStore::default().remove_admin_key(label)? {
        bail!("no admin key labeled {label:?}");
    }
    println!("Removed admin key: {label}");
    Ok(())
}

async fn attach_project(args: &[String]) -> anyhow::Result<()> {
    let label = args.first().ok_or_else(|| {
        anyhow!("usage: sr attach-project <api-key-label> [--project-id <id-or-name>]")
    })?;
    let store = CodexStore::default();
    let selector = if label.starts_with("apikey:") {
        label.clone()
    } else {
        format!("apikey:{label}")
    };
    let mut account = store
        .find_stored(&selector)?
        .ok_or_else(|| anyhow!("no API-key account named {label:?}"))?;
    if !account.is_api_key() {
        bail!("{label:?} is not an API-key account");
    }
    let admin = store
        .pick_admin_key_for(&account)?
        .ok_or_else(|| anyhow!("no admin key configured, run 'sr add-admin-key' first"))?;
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let projects = accounts::list_openai_projects(&client, &admin.key).await?;
    let requested = flag_value(&args[1..], "--project-id");
    let chosen = match requested.as_deref() {
        Some("none" | "org") => None,
        Some(selector) => Some(
            projects
                .iter()
                .find(|project| project.id == selector || project.name == selector)
                .cloned()
                .ok_or_else(|| anyhow!("no project matching {selector:?}"))?,
        ),
        None => {
            for (index, project) in projects.iter().enumerate() {
                println!("  {}) {}  {}", index + 1, project.name, project.id);
            }
            println!("  0) <none / org-wide>");
            let selected: usize = prompt_line("Pick project (#): ")?
                .parse()
                .context("invalid selection")?;
            if selected > projects.len() {
                bail!("invalid selection");
            }
            selected.checked_sub(1).map(|index| projects[index].clone())
        }
    };
    if let Some(project) = chosen {
        account.project_id = project.id;
        account.project_name = project.name;
    } else {
        account.project_id.clear();
        account.project_name.clear();
    }
    account.admin_key_label.clone_from(&admin.label);
    store.save_stored(&mut account)?;
    let scope = if account.project_name.is_empty() {
        "<org-wide>"
    } else {
        &account.project_name
    };
    println!(
        "Updated {}: project={scope} via admin={}",
        account.api_key_label(),
        admin.label
    );
    Ok(())
}

async fn usage(args: &[String]) -> anyhow::Result<()> {
    let days = args.first().map_or(Ok(30), |value| {
        value.parse::<u32>().context("days must be 1..30")
    })?;
    if !(1..=30).contains(&days) {
        bail!("days must be 1..30");
    }
    let store = CodexStore::default();
    let accounts = store
        .list_stored()?
        .into_iter()
        .filter(StoredCodexAccount::is_api_key)
        .collect::<Vec<_>>();
    if accounts.is_empty() {
        println!("No API-key accounts. Run 'sr add-key' first.");
        return Ok(());
    }
    if store.list_admin_keys()?.is_empty() {
        bail!("no admin keys. Run 'sr add-admin-key' first");
    }
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    println!(
        "Fetching {days}-day usage for {} API-key account(s)...",
        accounts.len()
    );
    for account in accounts {
        let admin = store
            .pick_admin_key_for(&account)?
            .ok_or_else(|| anyhow!("{}: no admin key linked", account.api_key_label()))?;
        match accounts::fetch_api_key_usage_snapshot(&client, &admin, &account, days).await {
            Ok(snapshot) => {
                store.write_usage_cache(&snapshot)?;
                print_spend_snapshot(&account, &snapshot);
            }
            Err(error) => println!(
                "{} via {}\terror: {error}",
                account.api_key_label(),
                admin.label
            ),
        }
    }
    Ok(())
}

fn print_spend_snapshot(account: &StoredCodexAccount, snapshot: &ApiKeyUsageSnapshot) {
    let model = snapshot
        .top_model
        .as_ref()
        .map(|model| format!(", top {}", model.model))
        .unwrap_or_default();
    println!(
        "{}\ttoday ${:.2}, 7d ${:.2}, 30d ${:.2}{model}",
        account.api_key_label(),
        snapshot.today_usd,
        snapshot.week_usd,
        snapshot.month_usd
    );
}

async fn claude_aws(args: &[String]) -> anyhow::Result<()> {
    let server = servers::Store::default().select(None)?.ok_or_else(|| {
        anyhow!("sr claude-aws needs a default Subrouter server; run 'sr server use <name>'")
    })?;
    let mut model = "fable".to_owned();
    let mut region = "us-east-1".to_owned();
    let mut passthrough = Vec::new();
    let mut index = 0;
    while index < args.len() {
        match args[index].as_str() {
            "--model" | "-m" => {
                index += 1;
                model = args
                    .get(index)
                    .ok_or_else(|| anyhow!("--model requires a value"))?
                    .clone();
            }
            "--aws-region" => {
                index += 1;
                region = args
                    .get(index)
                    .ok_or_else(|| anyhow!("--aws-region requires a value"))?
                    .clone();
            }
            argument => passthrough.push(argument.to_owned()),
        }
        index += 1;
    }
    let binary = claude::detect_cli()
        .ok_or_else(|| anyhow!("Claude CLI not found. Install from https://claude.ai/download"))?;
    let mut command = Command::new(binary);
    command
        .args(passthrough)
        .env("CLAUDE_CODE_USE_BEDROCK", "1")
        .env("CLAUDE_CODE_SKIP_BEDROCK_AUTH", "1")
        .env(
            "ANTHROPIC_BEDROCK_BASE_URL",
            format!("{}/bedrock", server.url.trim_end_matches('/')),
        )
        .env("AWS_REGION", &region)
        .env("AWS_DEFAULT_REGION", region)
        .env("ANTHROPIC_MODEL", bedrock_model_id(&model))
        .env(
            "ANTHROPIC_SMALL_FAST_MODEL",
            "us.anthropic.claude-haiku-4-5-20251001-v1:0",
        )
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    if let Some(token) = env::var("SUBROUTER_BEDROCK_GATEWAY_TOKEN")
        .ok()
        .filter(|token| !token.trim().is_empty())
    {
        command.env("ANTHROPIC_AUTH_TOKEN", token);
    }
    let status = command.status().await?;
    if !status.success() {
        bail!("Claude exited with {status}");
    }
    Ok(())
}

async fn claude_direct(args: &[String]) -> anyhow::Result<()> {
    let binary = claude::detect_cli()
        .ok_or_else(|| anyhow!("Claude CLI not found. Install from https://claude.ai/download"))?;
    let mut command = Command::new(binary);
    command
        .args(args)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    for key in CLAUDE_ROUTING_ENV_KEYS {
        command.env_remove(key);
    }
    let status = command.status().await?;
    if !status.success() {
        bail!("Claude exited with {status}");
    }
    Ok(())
}

fn bedrock_model_id(name: &str) -> String {
    let name = name.trim();
    if name.to_ascii_lowercase().contains("anthropic.") {
        return name.into();
    }
    match name.to_ascii_lowercase().as_str() {
        "" | "fable" | "fable-5" | "fable5" | "claude-fable-5" => {
            "us.anthropic.claude-fable-5".into()
        }
        "opus" | "claude-opus-4-8" | "opus-4-8" => "us.anthropic.claude-opus-4-8".into(),
        "sonnet" | "claude-sonnet-5" | "sonnet-5" => "us.anthropic.claude-sonnet-5".into(),
        "haiku" | "claude-haiku-4-5" => "us.anthropic.claude-haiku-4-5-20251001-v1:0".into(),
        _ => name.into(),
    }
}

async fn spend() -> anyhow::Result<()> {
    let server = servers::Store::default().select(None)?.ok_or_else(|| {
        anyhow!("sr spend needs a default Subrouter server; run 'sr server use <name>'")
    })?;
    let client = Client::builder().timeout(Duration::from_secs(15)).build()?;
    let response = authenticated_request(
        client.get(format!(
            "{}/_subrouter/bedrock-cost",
            server.control_base_url()
        )),
        &server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("bedrock cost fetch failed: {}", response.status());
    }
    let summary: BedrockSummary = response.json().await?;
    println!("AWS Bedrock spend (server {})", server.name);
    println!("  today     ${}", format_usd(summary.today_usd));
    println!("  last 7d   ${}", format_usd(summary.week_7d_usd));
    println!("  last 30d  ${}", format_usd(summary.month_30d_usd));
    println!(
        "  all-time  ${}   ({} requests)",
        format_usd(summary.total_usd),
        summary.requests
    );
    println!(
        "  tokens    {} in / {} out / {} cache-write / {} cache-read",
        summary.input_tokens,
        summary.output_tokens,
        summary.cache_write_tokens,
        summary.cache_read_tokens
    );
    let mut models = summary.by_model.into_iter().collect::<Vec<_>>();
    models.sort_by(|left, right| {
        right
            .1
            .total_usd
            .total_cmp(&left.1.total_usd)
            .then_with(|| left.0.cmp(&right.0))
    });
    if !models.is_empty() {
        println!("  by model:");
        for (model, cost) in models {
            println!(
                "    {model:26} ${}  ({} req)",
                format_usd(cost.total_usd),
                cost.requests
            );
        }
    }
    if summary.throttled > 0 {
        println!(
            "  throttled {} request(s) (429); last {}",
            summary.throttled, summary.last_throttle
        );
    }
    if summary.requests == 0 {
        println!("  no Bedrock requests recorded yet");
    }
    Ok(())
}

#[derive(Default, Deserialize)]
struct BedrockModelCost {
    requests: u64,
    total_usd: f64,
}

#[derive(Default, Deserialize)]
struct BedrockSummary {
    requests: u64,
    total_usd: f64,
    today_usd: f64,
    week_7d_usd: f64,
    month_30d_usd: f64,
    input_tokens: i64,
    output_tokens: i64,
    cache_read_tokens: i64,
    cache_write_tokens: i64,
    throttled: u64,
    #[serde(default)]
    last_throttle: String,
    #[serde(default)]
    by_model: std::collections::BTreeMap<String, BedrockModelCost>,
}

fn format_usd(value: f64) -> String {
    if value > 0.0 && value < 0.01 {
        format!("{value:.4}")
    } else {
        format!("{value:.2}")
    }
}

async fn remote_dispatch(server: &ServerConfig, args: &[String]) -> Option<anyhow::Result<()>> {
    let command = args.first().map(String::as_str)?;
    let result = match command {
        "add" => crate::remote::login_server(server, &args[1..]).await,
        "add-key" | "add-api-key" => crate::remote::add_remote_api_key(server, &args[1..]).await,
        "list" | "ls" => remote_accounts(server).await,
        "status" => remote_usage(server, None).await,
        "usage" if args.len() == 1 => remote_usage(server, None).await,
        "usage" => Err(anyhow!(
            "remote usage does not accept a day count; use 'sr server status {}'",
            server.name
        )),
        "pick" => remote_pick(server).await,
        "reset" => remote_reset(server, &args[1..]).await,
        "switch" | "use" | "g" | "gui" | "gui-switch" | "gui-use" if args.len() == 1 => {
            remote_usage(server, None).await
        }
        "switch" | "use" | "g" | "gui" | "gui-switch" | "gui-use" => Err(anyhow!(
            "remote servers select accounts per session; use SUBROUTER_CODEX_ACCOUNT_ID for a one-off forced account"
        )),
        "import" => Err(anyhow!(
            "copying a local refresh-token chain to a server is unsafe; use 'sr add' or 'sr server sync {}'",
            server.name
        )),
        "remove" | "rm" | "trace" | "breadcrumbs" | "why" | "add-admin-key" | "list-admin-keys"
        | "admin-keys" | "remove-admin-key" | "attach-project" => {
            Err(anyhow!("{command} has no remote-safe implementation"))
        }
        selector if selector.contains('@') => remote_usage(server, Some(selector)).await,
        _ => return None,
    };
    Some(result)
}

async fn remote_accounts(server: &ServerConfig) -> anyhow::Result<()> {
    let client = Client::builder().timeout(Duration::from_secs(30)).build()?;
    let response = authenticated_request(
        client.get(format!("{}/_subrouter/accounts", server.control_base_url())),
        server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("server accounts failed: {}", response.status());
    }
    let accounts: Vec<Value> = response.json().await?;
    println!("Server: {} ({})", server.name, server.url);
    for account in accounts {
        println!(
            "{}\t{}/{}",
            account
                .get("email")
                .and_then(Value::as_str)
                .or_else(|| account.get("id").and_then(Value::as_str))
                .unwrap_or("unknown"),
            account
                .get("provider")
                .and_then(Value::as_str)
                .unwrap_or("codex"),
            account
                .get("auth_mode")
                .and_then(Value::as_str)
                .unwrap_or("oauth")
        );
    }
    Ok(())
}

async fn remote_usage(server: &ServerConfig, selector: Option<&str>) -> anyhow::Result<()> {
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let response = authenticated_request(
        client.get(format!(
            "{}/_subrouter/usage-status",
            server.control_base_url()
        )),
        server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("server status failed: {}", response.status());
    }
    let entries: Vec<Value> = response.json().await?;
    let selector = selector.map(str::to_ascii_lowercase);
    let entries = entries
        .into_iter()
        .filter(|entry| {
            selector.as_ref().is_none_or(|selector| {
                entry
                    .get("email")
                    .and_then(Value::as_str)
                    .or_else(|| entry.get("id").and_then(Value::as_str))
                    .unwrap_or_default()
                    .to_ascii_lowercase()
                    .contains(selector)
            })
        })
        .collect::<Vec<_>>();
    if entries.is_empty() && selector.is_some() {
        bail!(
            "no server account found for {}",
            selector.unwrap_or_default()
        );
    }
    println!("Server: {} ({})", server.name, server.url);
    for entry in entries {
        let id = entry
            .get("email")
            .and_then(Value::as_str)
            .or_else(|| entry.get("id").and_then(Value::as_str))
            .unwrap_or("unknown");
        let error = entry
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if !error.is_empty() {
            println!("{id}\terror: {error}");
            continue;
        }
        let limits = entry
            .get("windows")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .map(|window| {
                format!(
                    "{} {:.0}% used",
                    window
                        .get("name")
                        .and_then(Value::as_str)
                        .unwrap_or("limit"),
                    window
                        .get("used_percent")
                        .and_then(Value::as_f64)
                        .unwrap_or_default()
                )
            })
            .collect::<Vec<_>>()
            .join(", ");
        println!("{id}\t{limits}");
    }
    Ok(())
}

async fn remote_pick(server: &ServerConfig) -> anyhow::Result<()> {
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let response = authenticated_request(
        client.get(format!(
            "{}/_subrouter/usage-status",
            server.control_base_url()
        )),
        server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("server status failed: {}", response.status());
    }
    let entries: Vec<Value> = response.json().await?;
    let mut candidates = entries
        .into_iter()
        .filter_map(|entry| {
            let id = entry
                .get("email")
                .and_then(Value::as_str)
                .or_else(|| entry.get("id").and_then(Value::as_str))?
                .to_owned();
            let headroom = entry
                .get("windows")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|window| window.get("used_percent").and_then(Value::as_f64))
                .map(|used| 1.0 - used.clamp(0.0, 100.0) / 100.0)
                .reduce(f64::min)
                .unwrap_or(1.0);
            Some((id, headroom))
        })
        .collect::<Vec<_>>();
    candidates.sort_by(|left, right| {
        right
            .1
            .total_cmp(&left.1)
            .then_with(|| left.0.cmp(&right.0))
    });
    let (id, headroom) = candidates
        .first()
        .ok_or_else(|| anyhow!("no Codex accounts configured on server {}", server.name))?;
    if *headroom < crate::selectacct::MIN_NEW_SESSION_HEADROOM {
        bail!("no recommended server account has quota for a new session");
    }
    println!("Server {} recommended for new sessions: {id}", server.name);
    Ok(())
}

async fn remote_reset(server: &ServerConfig, args: &[String]) -> anyhow::Result<()> {
    let options = parse_reset_options(args)?;
    if options.help {
        println!("usage: sr reset [email] [--all] [--gto [-n N]] [--list] [--dry-run]");
        return Ok(());
    }
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    if options.list {
        let response = authenticated_request(
            client.get(format!(
                "{}/_subrouter/reset-credits",
                server.control_base_url()
            )),
            server,
        )
        .send()
        .await?;
        if !response.status().is_success() {
            bail!("reset-credits failed: {}", response.status());
        }
        let value: Value = response.json().await?;
        let mut total = 0_u64;
        for entry in value
            .get("accounts")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let count = entry
                .get("count")
                .and_then(Value::as_u64)
                .unwrap_or_default();
            total += count;
            if count > 0 {
                println!(
                    "{}\t{count} credit(s)",
                    entry
                        .get("email")
                        .and_then(Value::as_str)
                        .unwrap_or("unknown")
                );
            }
        }
        println!("{total} reset credit(s).");
        return Ok(());
    }
    if options.gto {
        return remote_reset_gto(server, &client, options.count, options.dry_run).await;
    }
    let mut query = Vec::new();
    if !options.email.is_empty() {
        query.push(("email", options.email));
    }
    if options.all {
        query.push(("all", "true".into()));
    }
    if options.dry_run {
        query.push(("dry_run", "true".into()));
    }
    let response = authenticated_request(
        client
            .post(format!(
                "{}/_subrouter/rate-limit-reset",
                server.control_base_url()
            ))
            .query(&query),
        server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("rate-limit-reset failed: {}", response.status());
    }
    let value: Value = response.json().await?;
    println!(
        "{} {} account(s).",
        if options.dry_run {
            "Would reset"
        } else {
            "Reset"
        },
        value
            .get("reset")
            .and_then(Value::as_u64)
            .unwrap_or_default()
    );
    for result in value
        .get("results")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        println!(
            "  {}: {}",
            result
                .get("email")
                .and_then(Value::as_str)
                .unwrap_or("unknown"),
            result
                .get("error")
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .unwrap_or(if options.dry_run { "eligible" } else { "reset" })
        );
    }
    Ok(())
}

#[derive(Clone)]
struct RemoteResetCandidate {
    email: String,
    downtime: i64,
    weekly_headroom: f64,
    weekly_exhausted: bool,
    credits: u64,
}

async fn remote_reset_gto(
    server: &ServerConfig,
    client: &Client,
    count: usize,
    dry_run: bool,
) -> anyhow::Result<()> {
    let response = authenticated_request(
        client.get(format!(
            "{}/_subrouter/usage-status",
            server.control_base_url()
        )),
        server,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!("server status failed: {}", response.status());
    }
    let entries: Vec<Value> = response.json().await?;
    let mut usable = 0usize;
    let mut candidates = Vec::new();
    for entry in entries {
        if !entry
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .is_empty()
            || entry
                .get("provider")
                .and_then(Value::as_str)
                .is_some_and(|provider| provider != "codex")
            || entry
                .get("auth_mode")
                .and_then(Value::as_str)
                .is_some_and(|mode| mode != "oauth")
        {
            continue;
        }
        let email = entry
            .get("email")
            .and_then(Value::as_str)
            .or_else(|| entry.get("id").and_then(Value::as_str))
            .unwrap_or_default()
            .trim()
            .to_owned();
        if email.is_empty() {
            continue;
        }
        let windows = entry
            .get("windows")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let cooked = windows.iter().any(|window| {
            !remote_spark_window(window)
                && window
                    .get("used_percent")
                    .and_then(Value::as_f64)
                    .unwrap_or_default()
                    >= 100.0
        });
        if !cooked {
            usable += 1;
            continue;
        }
        let reset = entry.get("complimentary_reset");
        let available = reset
            .and_then(|reset| reset.get("available"))
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let credits = reset
            .and_then(|reset| reset.get("remaining"))
            .and_then(Value::as_u64)
            .unwrap_or(u64::from(available));
        if !available || credits == 0 {
            continue;
        }
        let mut downtime = 0i64;
        let mut weekly_headroom = 1.0f64;
        let mut weekly_exhausted = false;
        for window in &windows {
            if remote_spark_window(window) {
                continue;
            }
            let used = window
                .get("used_percent")
                .and_then(Value::as_f64)
                .unwrap_or_default()
                .clamp(0.0, 100.0);
            let seconds = window
                .get("limit_window_seconds")
                .and_then(Value::as_i64)
                .unwrap_or_default();
            if seconds > 6 * 60 * 60 {
                weekly_headroom = weekly_headroom.min(1.0 - used / 100.0);
                weekly_exhausted |= used >= 100.0;
            }
            if used >= 100.0 {
                downtime = downtime.max(
                    window
                        .get("reset_after_seconds")
                        .and_then(Value::as_i64)
                        .unwrap_or_default(),
                );
            }
        }
        candidates.push(RemoteResetCandidate {
            email,
            downtime,
            weekly_headroom,
            weekly_exhausted,
            credits,
        });
    }
    candidates.sort_by(|left, right| {
        left.weekly_exhausted
            .cmp(&right.weekly_exhausted)
            .then_with(|| right.weekly_headroom.total_cmp(&left.weekly_headroom))
            .then_with(|| right.downtime.cmp(&left.downtime))
            .then_with(|| left.email.cmp(&right.email))
    });
    if usable > 0 {
        println!(
            "LOW VALUE: {usable} Codex account(s) already usable; no reset is needed to unblock."
        );
    } else if let Some(soonest) = candidates.iter().map(|candidate| candidate.downtime).min() {
        if soonest > 0 && soonest < 20 * 60 {
            println!(
                "LOW VALUE: every cooked account self-heals within {}; a reset saves little.",
                humantime::format_duration(Duration::from_secs(soonest as u64))
            );
        } else if soonest > 0 {
            println!(
                "GOOD VALUE: all Codex accounts are cooked; soonest natural recovery is in {}.",
                humantime::format_duration(Duration::from_secs(soonest as u64))
            );
        } else {
            println!("GOOD VALUE: all Codex accounts are cooked with no near-term natural reset.");
        }
    } else {
        println!("No cooked Codex account holds a reset credit.");
    }
    if candidates.is_empty() {
        if dry_run {
            return Ok(());
        }
        bail!("no cooked Codex account has a rate-limit reset credit available");
    }
    let selected = candidates.into_iter().take(count).collect::<Vec<_>>();
    if dry_run {
        println!("Top {} reset candidate(s):", selected.len());
        for (index, candidate) in selected.iter().enumerate() {
            let saved = if candidate.downtime > 0 {
                format!(
                    "saves {}",
                    humantime::format_duration(Duration::from_secs(candidate.downtime as u64))
                )
            } else {
                "self-heals now".into()
            };
            let warning = if candidate.weekly_exhausted {
                " (weekly maxed; reset may not fully restore quota)"
            } else {
                ""
            };
            println!(
                "  {}. {}: {:.0}% weekly headroom after reset, {saved}, {} credit(s) left{warning}",
                index + 1,
                candidate.email,
                candidate.weekly_headroom * 100.0,
                candidate.credits
            );
        }
        return Ok(());
    }
    let mut total = 0u64;
    for candidate in selected {
        let response = authenticated_request(
            client
                .post(format!(
                    "{}/_subrouter/rate-limit-reset",
                    server.control_base_url()
                ))
                .query(&[("email", &candidate.email)]),
            server,
        )
        .send()
        .await?;
        if !response.status().is_success() {
            println!(
                "  {}: reset failed ({})",
                candidate.email,
                response.status()
            );
            continue;
        }
        let value: Value = response.json().await?;
        total += value
            .get("reset")
            .and_then(Value::as_u64)
            .unwrap_or_default();
        for result in value
            .get("results")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let error = result
                .get("error")
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .unwrap_or("reset");
            println!("  {}: {error}", candidate.email);
        }
    }
    println!("Reset {total} account(s).");
    Ok(())
}

fn remote_spark_window(window: &Value) -> bool {
    ["name", "feature"]
        .into_iter()
        .filter_map(|field| window.get(field).and_then(Value::as_str))
        .any(|value| value.to_ascii_lowercase().contains("spark"))
}

fn authenticated_request(
    builder: reqwest::RequestBuilder,
    server: &ServerConfig,
) -> reqwest::RequestBuilder {
    if server.admin_token.is_empty() {
        builder
    } else {
        builder.bearer_auth(&server.admin_token)
    }
}

fn flag_value(args: &[String], name: &str) -> Option<String> {
    args.iter().enumerate().find_map(|(index, value)| {
        value
            .strip_prefix(&format!("{name}="))
            .map(str::to_owned)
            .or_else(|| {
                (value == name)
                    .then(|| args.get(index + 1).cloned())
                    .flatten()
            })
    })
}

fn prompt_line(prompt: &str) -> anyhow::Result<String> {
    print!("{prompt}");
    io::stdout().flush()?;
    let mut value = String::new();
    io::stdin().lock().read_line(&mut value)?;
    let value = value.trim().to_owned();
    if value.is_empty() {
        bail!("value is required");
    }
    Ok(value)
}

fn prompt_secret(prompt: &str) -> anyhow::Result<String> {
    let value = if io::stdin().is_terminal() {
        rpassword::prompt_password(prompt)?
    } else {
        prompt_line(prompt)?
    };
    if value.trim().is_empty() {
        bail!("value is required");
    }
    Ok(value.trim().into())
}

fn mask_secret(value: &str) -> String {
    if value.len() <= 10 {
        "***".into()
    } else {
        format!("{}...{}", &value[..6], &value[value.len() - 4..])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bedrock_aliases_are_stable() {
        assert_eq!(bedrock_model_id("fable"), "us.anthropic.claude-fable-5");
        assert_eq!(
            bedrock_model_id("us.anthropic.custom"),
            "us.anthropic.custom"
        );
    }

    #[test]
    fn reset_options_enforce_exclusive_modes() {
        assert!(parse_reset_options(&["x@example.com".into(), "--all".into()]).is_err());
        assert!(parse_reset_options(&["--gto".into(), "-n".into(), "0".into()]).is_err());
        let options =
            parse_reset_options(&["--gto".into(), "-n".into(), "2".into(), "--dry-run".into()])
                .unwrap();
        assert_eq!(options.count, 2);
        assert!(options.gto && options.dry_run);
    }

    #[test]
    fn secrets_are_masked() {
        let masked = mask_secret("sk-admin-abcdefghijkl");
        assert!(!masked.contains("abcdefgh"));
        assert!(masked.ends_with("ijkl"));
    }
}
