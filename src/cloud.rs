//! cmux.com login, team selection, and shared credential management.

use std::collections::HashSet;
use std::env;
use std::fs;
use std::io::{self, IsTerminal as _, Write as _};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use serde_json::{Map, Value, json};

use crate::accounts::CodexStore;
use crate::agents::claude;
use crate::broker::{self, BrokerClient, Config, CredentialSource, Team};
use crate::{servers, service};

pub async fn maybe_dispatch(args: &[String]) -> Option<anyhow::Result<()>> {
    let command = args.first()?.as_str();
    let always = matches!(
        command,
        "login"
            | "logout"
            | "storage"
            | "team"
            | "account"
            | "accounts"
            | "setup"
            | "doctor"
            | "cleanup"
    );
    if always {
        return Some(dispatch(command, &args[1..]).await);
    }
    let config = match broker::load_config(None) {
        Ok(config) => config,
        Err(error) => return Some(Err(error.context("load credential storage"))),
    };
    if config.effective_credential_source() != CredentialSource::Team {
        return None;
    }
    let result = match command {
        "list" | "ls" | "status" | "usage" => status().await,
        "add" => account_add(&["codex".into()]).await,
        "add-key" | "add-api-key" => account_add(&["openai-key".into()]).await,
        "import" => account_import(&args[1..]).await,
        "remove" | "rm" => account_command(args).await,
        "switch" | "use" | "g" | "gui" | "gui-switch" | "gui-use" | "pick" | "reset" => {
            Err(anyhow!(
                "team storage selects an account per request; use 'sr account list' or switch to local storage with 'sr storage local'"
            ))
        }
        _ => return None,
    };
    Some(result)
}

async fn dispatch(command: &str, args: &[String]) -> anyhow::Result<()> {
    match command {
        "login" => login(args).await,
        "logout" => logout().await,
        "storage" => storage(args).await,
        "team" => team_command(args).await,
        "account" | "accounts" => account_command(args).await,
        "setup" => setup(args).await,
        "doctor" => doctor().await,
        "cleanup" => cleanup(args).await,
        _ => unreachable!(),
    }
}

async fn login(args: &[String]) -> anyhow::Result<()> {
    let path = broker::default_config_path()?;
    let mut config = load_for_login(&path);
    if let Some(base_url) = flag_value(args, "--base-url") {
        config.base_url = base_url;
    }
    let requested_team = flag_value(args, "--team").unwrap_or_default();
    let no_browser = flag_present(args, "--no-browser");
    let client = BrokerClient::new(config.clone())?;
    let start = client.start_auth().await.context("start cmux.com login")?;
    println!(
        "Approve Subrouter at:\n  {}\n\nCode: {}",
        start.verification_url, start.user_code
    );
    if !no_browser {
        open_browser(&start.verification_url);
    }
    let interval = Duration::from_secs(u64::try_from(start.interval_seconds.max(1)).unwrap_or(3));
    let timeout =
        Duration::from_secs(u64::try_from(start.expires_in_seconds.max(1)).unwrap_or(900));
    let deadline = tokio::time::Instant::now() + timeout;
    let approved = loop {
        if tokio::time::Instant::now() >= deadline {
            bail!("login expired before approval");
        }
        tokio::time::sleep(interval).await;
        let poll = client
            .poll_auth(&start.device_code)
            .await
            .context("poll cmux.com login")?;
        match poll.status.as_str() {
            "pending" => continue,
            "approved"
                if poll.client == "subrouter"
                    && !poll.access_token.is_empty()
                    && !poll.refresh_token.is_empty() =>
            {
                break poll;
            }
            "approved" => bail!("cmux.com returned an invalid Subrouter approval"),
            status => bail!("login {status}"),
        }
    };
    config.access_token = approved.access_token;
    config.refresh_token = approved.refresh_token;
    let client = BrokerClient::new(config.clone())?;
    let (teams, selected) = client.list_teams().await?;
    let selector = if requested_team.is_empty() {
        selected
    } else {
        requested_team
    };
    let team = match_team(&teams, &selector)?;
    config.team_id.clone_from(&team.id);
    config.team_name.clone_from(&team.name);
    config.credential_source = CredentialSource::Team;
    broker::save_config(Some(&path), &config)?;
    println!("Logged in to cmux.com team {} ({}).", team.name, team.id);
    service::restart_if_installed().await
}

async fn logout() -> anyhow::Result<()> {
    let path = broker::default_config_path()?;
    let mut config = load_for_login(&path);
    if config.logged_in() {
        BrokerClient::new(config.clone())?
            .logout()
            .await
            .context("revoke cmux.com session")?;
    }
    config.access_token.clear();
    config.refresh_token.clear();
    config.team_id.clear();
    config.team_name.clear();
    config.credential_source = CredentialSource::Local;
    broker::save_config(Some(&path), &config)?;
    println!("Logged out of cmux.com. Credential storage is now local.");
    service::restart_if_installed().await
}

async fn team_command(args: &[String]) -> anyhow::Result<()> {
    let (mut config, path, client) = client(true)?;
    match args.first().map(String::as_str).unwrap_or("current") {
        "list" | "ls" => {
            let (teams, _) = client.list_teams().await?;
            for team in teams {
                println!(
                    "{} {:28} {}",
                    if team.id == config.team_id { "*" } else { " " },
                    team.name,
                    team.id
                );
            }
            Ok(())
        }
        "current" => {
            if config.team_id.is_empty() {
                bail!("no team selected; run 'sr team list' then 'sr team use <team>'");
            }
            println!("{} ({})", config.team_name, config.team_id);
            Ok(())
        }
        "use" => {
            let selector = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr team use <team-id-or-name>"))?;
            let (teams, _) = client.list_teams().await?;
            let team = match_team(&teams, selector)?;
            config.team_id.clone_from(&team.id);
            config.team_name.clone_from(&team.name);
            config.credential_source = CredentialSource::Team;
            broker::save_config(Some(&path), &config)?;
            println!("Using {} ({}).", team.name, team.id);
            service::restart_if_installed().await
        }
        command => bail!("unknown team command {command:?}; use list, current, or use"),
    }
}

async fn storage(args: &[String]) -> anyhow::Result<()> {
    let args = if args.first().is_some_and(|value| value == "use") {
        &args[1..]
    } else {
        args
    };
    let mut config = broker::load_config(None)?;
    if args.is_empty() || matches!(args[0].as_str(), "current" | "status") {
        return print_storage(&config);
    }
    if args.len() != 1 {
        bail!("usage: sr storage [team|local|legacy]");
    }
    let source = match args[0].to_ascii_lowercase().as_str() {
        "team" | "shared" | "cloud" => CredentialSource::Team,
        "local" | "device" | "machine" => CredentialSource::Local,
        "legacy" | "server" | "remote" => CredentialSource::Legacy,
        _ => bail!("credential storage must be team, local, or legacy"),
    };
    if source == CredentialSource::Team && !config.ready() {
        bail!("team credential storage requires login and a selected team; run 'sr login'");
    }
    config.credential_source = source;
    broker::save_config(None, &config)?;
    print_storage(&config)?;
    service::restart_if_installed().await
}

fn print_storage(config: &Config) -> anyhow::Result<()> {
    match config.effective_credential_source() {
        CredentialSource::Team if config.ready() => println!(
            "Credential storage: team ({}, {})",
            config.team_name, config.team_id
        ),
        CredentialSource::Team => {
            bail!("team credential storage requires login and a selected team; run 'sr login'")
        }
        CredentialSource::Local => println!(
            "Credential storage: local ({})",
            CodexStore::default().store_dir().display()
        ),
        CredentialSource::Legacy | CredentialSource::Unspecified => {
            println!("Credential storage: legacy remote server")
        }
    }
    Ok(())
}

async fn status() -> anyhow::Result<()> {
    let (config, _, client) = client(true)?;
    print_storage(&config)?;
    println!("\nShared accounts");
    print_accounts(&client.list_accounts().await?);
    Ok(())
}

async fn account_command(args: &[String]) -> anyhow::Result<()> {
    let (_, _, client) = client(true)?;
    match args.first().map(String::as_str).unwrap_or("list") {
        "list" | "ls" => {
            print_accounts(&client.list_accounts().await?);
            Ok(())
        }
        "import" | "push" | "upload" => account_import(&args[1..]).await,
        "remove" | "rm" => {
            let id = args
                .get(1)
                .ok_or_else(|| anyhow!("usage: sr account remove <account-id>"))?;
            client.delete_account(id).await?;
            println!("Removed shared account {id}.");
            service::restart_if_installed().await
        }
        "add" => account_add(&args[1..]).await,
        "repair" => account_repair(&args[1..]).await,
        command => {
            bail!("unknown account command {command:?}; use list, import, add, remove, or repair")
        }
    }
}

async fn account_add(args: &[String]) -> anyhow::Result<()> {
    let provider = args.first().map(String::as_str).unwrap_or("codex");
    let (_, _, client) = client(true)?;
    if provider == "anthropic-key" {
        let label = prompt("Label: ", false)?;
        let key = prompt("Anthropic API key (sk-ant-...): ", true)?;
        if !key.starts_with("sk-ant-") {
            bail!("Anthropic API key must start with sk-ant-");
        }
        client
            .upload_account(Map::from_iter([
                ("provider".into(), "anthropic-apikey".into()),
                ("label".into(), label.clone().into()),
                ("apiKey".into(), key.into()),
            ]))
            .await?;
        println!("Added shared Anthropic API key: {label}");
        return service::restart_if_installed().await;
    }
    let before: HashSet<_> = local_uploads()
        .await?
        .iter()
        .map(LocalUpload::key)
        .collect();
    match provider {
        "codex" => crate::cli::add_account(&CodexStore::default(), "codex").await?,
        "claude" => crate::cli::add_account(&CodexStore::default(), "claude").await?,
        "openai-key" => crate::cli::add_key(&CodexStore::default(), &[])?,
        _ => {
            bail!("unknown provider {provider:?}; use codex, claude, openai-key, or anthropic-key")
        }
    }
    let added: Vec<_> = local_uploads()
        .await?
        .into_iter()
        .filter(|upload| !before.contains(&upload.key()))
        .collect();
    if added.len() != 1 {
        bail!(
            "authentication created {} new credentials; use 'sr account import --only <label>'",
            added.len()
        );
    }
    upload_one(&client, &added[0], true).await?;
    service::restart_if_installed().await
}

pub(crate) async fn account_import(args: &[String]) -> anyhow::Result<()> {
    let all = flag_present(args, "--all");
    let only = flag_value(args, "--only").unwrap_or_default();
    let dry_run = flag_present(args, "--dry-run");
    let yes = flag_present(args, "--yes");
    if all == !only.is_empty() {
        bail!("usage: sr account import (--only <label|kind:label> | --all) [--dry-run] [--yes]");
    }
    let (_, _, client) = client(true)?;
    let mut uploads = local_uploads().await?;
    if !all {
        uploads = select_upload(uploads, &only)?;
    }
    let existing: HashSet<_> = client
        .list_accounts()
        .await?
        .into_iter()
        .map(|item| shared_key(&item.kind, &item.label))
        .collect();
    uploads.retain(|upload| !existing.contains(&upload.key()));
    uploads.sort_by(|left, right| {
        left.kind
            .cmp(&right.kind)
            .then_with(|| left.label.cmp(&right.label))
    });
    if uploads.is_empty() {
        println!("All selected local accounts are already shared.");
        return Ok(());
    }
    for upload in &uploads {
        println!("{:<20} {}", upload.kind, upload.label);
    }
    if dry_run {
        println!("\n{} account(s) would be uploaded.", uploads.len());
        return Ok(());
    }
    if all && !yes {
        bail!("bulk import requires --yes after reviewing 'sr account import --all --dry-run'");
    }
    for upload in &uploads {
        upload_one(&client, upload, true).await?;
        println!("uploaded {:<20} {}", upload.kind, upload.label);
    }
    println!("\nUploaded {} shared account(s).", uploads.len());
    service::restart_if_installed().await
}

async fn upload_one(
    client: &BrokerClient,
    upload: &LocalUpload,
    hand_over: bool,
) -> anyhow::Result<()> {
    client.upload_account(upload.body.clone()).await?;
    if hand_over
        && upload.kind == "codex"
        && let Some(path) = CodexStore::default().migrate_stored_away(&upload.label)?
    {
        println!(
            "  local copy kept as a rollback record at {}",
            path.display()
        );
    }
    Ok(())
}

async fn account_repair(args: &[String]) -> anyhow::Result<()> {
    let id = args
        .first()
        .ok_or_else(|| anyhow!("usage: sr account repair <account-id>"))?;
    let (_, _, client) = client(true)?;
    let target = client
        .list_accounts()
        .await?
        .into_iter()
        .find(|account| account.id == *id)
        .ok_or_else(|| anyhow!("shared account {id:?} not found"))?;
    let matches: Vec<_> = local_uploads()
        .await?
        .into_iter()
        .filter(|upload| upload.key() == shared_key(&target.kind, &target.label))
        .collect();
    let body = match matches.as_slice() {
        [upload] => upload.body.clone(),
        [] if matches!(target.kind.as_str(), "openai-apikey" | "anthropic-apikey") => {
            let prefix = if target.kind == "anthropic-apikey" {
                "sk-ant-"
            } else {
                "sk-"
            };
            let key = prompt(&format!("Replacement API key for {}: ", target.label), true)?;
            if !key.starts_with(prefix) {
                bail!("{} API key must start with {prefix}", target.kind);
            }
            Map::from_iter([
                ("provider".into(), target.kind.clone().into()),
                ("label".into(), target.label.clone().into()),
                ("apiKey".into(), key.into()),
            ])
        }
        [] => bail!(
            "no matching local credential for {}; authenticate it locally first",
            target.label
        ),
        _ => bail!(
            "multiple local credentials match {}; remove the duplicate before repair",
            target.label
        ),
    };
    client.repair_account(id, body).await?;
    println!("Repaired shared account {} ({}).", target.label, id);
    service::restart_if_installed().await
}

#[derive(Clone)]
struct LocalUpload {
    kind: String,
    label: String,
    body: Map<String, Value>,
}

impl LocalUpload {
    fn key(&self) -> String {
        shared_key(&self.kind, &self.label)
    }
}

async fn local_uploads() -> anyhow::Result<Vec<LocalUpload>> {
    let store = CodexStore::default();
    let mut uploads = Vec::new();
    for account in store.list_stored()? {
        if account.is_api_key() {
            if account.auth.openai_api_key.is_empty() {
                continue;
            }
            let kind = match account.provider_or_default() {
                crate::account::Provider::Codex => "openai-apikey",
                crate::account::Provider::Claude => "anthropic-apikey",
                crate::account::Provider::Kimi | crate::account::Provider::Zai => continue,
            };
            uploads.push(LocalUpload {
                kind: kind.into(),
                label: account.email.clone(),
                body: Map::from_iter([
                    ("provider".into(), kind.into()),
                    ("label".into(), account.email.into()),
                    ("apiKey".into(), account.auth.openai_api_key.into()),
                ]),
            });
        } else if let Some(tokens) = account.auth.tokens.filter(|tokens| {
            !tokens.access_token.is_empty()
                && !tokens.refresh_token.is_empty()
                && !tokens.id_token.is_empty()
        }) {
            uploads.push(LocalUpload {
                kind: "codex".into(),
                label: account.email.clone(),
                body: Map::from_iter([
                    ("provider".into(), "codex".into()),
                    ("label".into(), account.email.into()),
                    (
                        "tokens".into(),
                        json!({
                            "accessToken": tokens.access_token,
                            "refreshToken": tokens.refresh_token,
                            "idToken": tokens.id_token,
                            "accountID": tokens.account_id,
                        }),
                    ),
                ]),
            });
        }
    }
    let claude_store = claude::Store::default();
    let mut seen = HashSet::new();
    for profile in claude_store.list_profiles() {
        let Some(credential) = claude_store
            .read_credential(&claude_store.claude_config_dir(&profile.name))
            .await?
        else {
            continue;
        };
        if let Some(upload) = claude_upload(&profile.name, credential) {
            seen.insert(
                upload.body["claudeAiOauth"]["refreshToken"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned(),
            );
            uploads.push(upload);
        }
    }
    if let Some(home) = env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        && let Ok(Some(credential)) = claude_store
            .read_credential(&PathBuf::from(home).join(".claude"))
            .await
        && !seen.contains(&credential.refresh_token)
        && let Some(upload) = claude_upload("default", credential)
    {
        uploads.push(upload);
    }
    Ok(uploads)
}

fn claude_upload(label: &str, credential: claude::CredentialInfo) -> Option<LocalUpload> {
    if credential.access_token.is_empty() || credential.refresh_token.is_empty() {
        return None;
    }
    let expires_at = if credential.expires_at <= 0 {
        chrono::Utc::now().timestamp_millis()
    } else {
        credential.expires_at
    };
    Some(LocalUpload {
        kind: "claude".into(),
        label: label.into(),
        body: Map::from_iter([
            ("provider".into(), "claude".into()),
            ("label".into(), label.into()),
            (
                "claudeAiOauth".into(),
                json!({
                    "accessToken": credential.access_token,
                    "refreshToken": credential.refresh_token,
                    "expiresAt": expires_at,
                    "subscriptionType": credential.subscription_type,
                    "rateLimitTier": credential.rate_limit_tier,
                }),
            ),
        ]),
    })
}

fn select_upload(uploads: Vec<LocalUpload>, selector: &str) -> anyhow::Result<Vec<LocalUpload>> {
    let selector = selector.trim().to_ascii_lowercase();
    if selector.is_empty() {
        bail!("account selector cannot be empty");
    }
    let exact: Vec<_> = uploads
        .iter()
        .filter(|upload| {
            upload.label.eq_ignore_ascii_case(&selector)
                || format!("{}:{}", upload.kind, upload.label).eq_ignore_ascii_case(&selector)
        })
        .cloned()
        .collect();
    let matches = if exact.is_empty() {
        uploads
            .into_iter()
            .filter(|upload| {
                upload.label.to_ascii_lowercase().contains(&selector)
                    || format!("{}:{}", upload.kind, upload.label)
                        .to_ascii_lowercase()
                        .contains(&selector)
            })
            .collect::<Vec<_>>()
    } else {
        exact
    };
    match matches.len() {
        0 => bail!("local account {selector:?} not found"),
        1 => Ok(matches),
        _ => bail!("local account {selector:?} is ambiguous; use kind:label"),
    }
}

async fn setup(args: &[String]) -> anyhow::Result<()> {
    let plan = flag_present(args, "--plan");
    let yes = flag_present(args, "--yes");
    let no_login = flag_present(args, "--no-login");
    let no_install = flag_present(args, "--no-install") || flag_present(args, "--no-background");
    let no_config = flag_present(args, "--no-config");
    let requested_storage = flag_value(args, "--storage");
    let mut config = broker::load_config(None).unwrap_or_else(|_| Config {
        version: 1,
        base_url: broker::DEFAULT_BASE_URL.into(),
        ..Config::default()
    });
    let source = match requested_storage.as_deref() {
        Some("local") => CredentialSource::Local,
        Some("team") => CredentialSource::Team,
        Some(value) => bail!("credential storage must be team or local, got {value:?}"),
        None if no_login && !config.ready() => CredentialSource::Local,
        None if config.credential_source == CredentialSource::Unspecified && !config.ready() => {
            CredentialSource::Team
        }
        None => config.effective_credential_source(),
    };
    if plan || (!yes && !io::stdin().is_terminal()) {
        println!("Subrouter setup will:");
        println!(
            "  {} use {} credential storage",
            if config.effective_credential_source() == source {
                "="
            } else {
                "~"
            },
            source_name(source)
        );
        println!(
            "  {} install the per-user daemon",
            if service::daemon_installed() || no_install {
                "="
            } else {
                "+"
            }
        );
        println!(
            "  {} configure Codex and Claude Code for http://127.0.0.1:31415",
            if no_config { "-" } else { "+" }
        );
        if !plan && !yes {
            println!("\nnoninteractive terminal: re-run with --yes to apply");
        }
        return Ok(());
    }
    if source == CredentialSource::Team && !config.logged_in() && !no_login {
        println!("First, sign in to cmux.com.");
        login(&[]).await?;
        config = broker::load_config(None)?;
    }
    if source == CredentialSource::Team && !config.ready() {
        bail!("team credential storage requires login and a selected team; run 'sr login'");
    }
    config.credential_source = source;
    broker::save_config(None, &config)?;
    if !no_install {
        service::install_for_current_user().await?;
    } else if service::daemon_installed() {
        service::restart_if_installed().await?;
    } else {
        bail!("no local daemon installed; rerun without --no-install");
    }
    if !no_config {
        let path = servers::write_codex_config(None)?;
        println!("Updated Codex routing: {}", path.display());
        configure_claude_local()?;
    }
    wait_local_health().await?;
    if source == CredentialSource::Team {
        let shared = BrokerClient::new(config.clone())?.list_accounts().await?;
        println!(
            "{} shared account(s) available from {}.",
            shared.len(),
            config.team_name
        );
    } else {
        println!(
            "{} local account(s) available.",
            CodexStore::default().list()?.len()
        );
    }
    Ok(())
}

fn configure_claude_local() -> anyhow::Result<()> {
    let home = env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .ok_or_else(|| anyhow!("home directory unavailable"))?;
    let path = PathBuf::from(home).join(".claude/settings.json");
    let mut root: Map<String, Value> = fs::read(&path)
        .ok()
        .and_then(|body| serde_json::from_slice(&body).ok())
        .unwrap_or_default();
    let mut vars = root
        .remove("env")
        .and_then(|value| value.as_object().cloned())
        .unwrap_or_default();
    vars.insert("ANTHROPIC_BASE_URL".into(), "http://127.0.0.1:31415".into());
    vars.entry("ANTHROPIC_AUTH_TOKEN")
        .or_insert_with(|| "subrouter".into());
    root.insert("env".into(), Value::Object(vars));
    let mut body = serde_json::to_vec_pretty(&root)?;
    body.push(b'\n');
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    crate::agents::opencode::write_atomic_private(&path, &body)?;
    println!("Updated Claude routing: {}", path.display());
    Ok(())
}

async fn doctor() -> anyhow::Result<()> {
    let config = broker::load_config(None)?;
    let mut failed = false;
    match config.effective_credential_source() {
        CredentialSource::Team if !config.ready() => {
            println!("FAIL  team vault           not logged in or no team selected");
            failed = true;
        }
        CredentialSource::Team => println!(
            "ok    team vault           {} ({})",
            config.team_name, config.team_id
        ),
        CredentialSource::Local => println!(
            "ok    credential storage   local ({})",
            CodexStore::default().store_dir().display()
        ),
        _ => println!("warn  credential storage   legacy remote server"),
    }
    println!(
        "{}  daemon installed     {}",
        if service::daemon_installed() {
            "ok  "
        } else {
            "warn"
        },
        if service::daemon_installed() {
            "yes"
        } else {
            "no; run 'sr setup'"
        }
    );
    let health = reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .build()?
        .get("http://127.0.0.1:31415/_subrouter/health")
        .send()
        .await
        .is_ok_and(|response| response.status().is_success());
    println!(
        "{}  local daemon         http://127.0.0.1:31415",
        if health {
            "ok  "
        } else {
            failed = true;
            "FAIL"
        }
    );
    let count = if config.team_mode_ready() {
        BrokerClient::new(config)?.list_accounts().await?.len()
    } else {
        CodexStore::default().list()?.len()
    };
    if count == 0 {
        println!("FAIL  accounts             none; run 'sr add'");
        failed = true;
    } else {
        println!("ok    accounts             {count} available");
    }
    if failed {
        bail!("doctor found a blocking problem")
    } else {
        Ok(())
    }
}

async fn cleanup(args: &[String]) -> anyhow::Result<()> {
    let yes = flag_present(args, "--yes");
    let purge = flag_present(args, "--purge");
    let store = CodexStore::default();
    println!("cleanup will:");
    if service::daemon_installed() {
        println!("  - stop and remove the local daemon");
    }
    if purge {
        println!(
            "  - delete {} and the cmux.com session",
            store.store_dir().display()
        );
    }
    if !yes {
        println!("\nre-run with --yes to proceed");
        return Ok(());
    }
    if service::remove_installed()? {
        println!("removed local daemon");
    }
    if purge {
        let path = broker::default_config_path()?;
        if let Ok(config) = broker::load_config(Some(&path))
            && config.logged_in()
        {
            BrokerClient::new(config)?
                .logout()
                .await
                .context("revoke cmux.com session before purge")?;
        }
        broker::delete_config(Some(&path))?;
        let target = store.store_dir();
        if target.exists() {
            fs::remove_dir_all(&target)?;
        }
        println!("deleted {}", target.display());
    }
    Ok(())
}

fn client(require_team: bool) -> anyhow::Result<(Config, PathBuf, BrokerClient)> {
    let path = broker::default_config_path()?;
    let config = broker::load_config(Some(&path))?;
    if !config.logged_in() {
        bail!("not logged in; run 'sr login'");
    }
    if require_team && config.team_id.is_empty() {
        bail!("no team selected; run 'sr team use <team>'");
    }
    let client = BrokerClient::new(config.clone())?;
    Ok((config, path, client))
}

fn load_for_login(path: &Path) -> Config {
    broker::load_config(Some(path)).unwrap_or_else(|_| Config {
        version: 1,
        base_url: broker::DEFAULT_BASE_URL.into(),
        ..Config::default()
    })
}

fn match_team(teams: &[Team], selector: &str) -> anyhow::Result<Team> {
    let usable: Vec<_> = teams
        .iter()
        .filter(|team| team.use_permission)
        .cloned()
        .collect();
    if selector.trim().is_empty() {
        return match usable.as_slice() {
            [team] => Ok(team.clone()),
            _ => bail!("choose a team with 'sr team use <team>'"),
        };
    }
    if let Some(team) = teams.iter().find(|team| team.id == selector) {
        if !team.use_permission {
            bail!("team {selector:?} does not grant Subrouter use permission")
        }
        return Ok(team.clone());
    }
    let lower = selector.to_ascii_lowercase();
    let exact: Vec<_> = usable
        .iter()
        .filter(|team| team.name.eq_ignore_ascii_case(selector))
        .cloned()
        .collect();
    let matches = if exact.is_empty() {
        usable
            .into_iter()
            .filter(|team| {
                team.name.to_ascii_lowercase().contains(&lower)
                    || team.id.to_ascii_lowercase().contains(&lower)
            })
            .collect::<Vec<_>>()
    } else {
        exact
    };
    match matches.as_slice() {
        [team] => Ok(team.clone()),
        [] => bail!("team {selector:?} not found"),
        _ => bail!("team {selector:?} is ambiguous"),
    }
}

fn print_accounts(accounts: &[broker::SharedAccount]) {
    if accounts.is_empty() {
        println!("No shared accounts.");
        return;
    }
    for account in accounts {
        let status = if account.health.as_ref().is_some_and(|health| !health.ok) {
            "NEEDS REPAIR"
        } else {
            "ready"
        };
        println!(
            "{:<20} {:<32} {:<14} {}",
            account.kind, account.label, status, account.id
        );
    }
}

fn source_name(source: CredentialSource) -> &'static str {
    match source {
        CredentialSource::Team => "team",
        CredentialSource::Local => "local",
        CredentialSource::Legacy | CredentialSource::Unspecified => "legacy",
    }
}

async fn wait_local_health() -> anyhow::Result<()> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(1))
        .build()?;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(15);
    while tokio::time::Instant::now() < deadline {
        if client
            .get("http://127.0.0.1:31415/_subrouter/health")
            .send()
            .await
            .is_ok_and(|response| response.status().is_success())
        {
            println!("daemon healthy at http://127.0.0.1:31415");
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    bail!("daemon did not become healthy at http://127.0.0.1:31415 within 15s")
}

fn open_browser(url: &str) {
    let program = match env::consts::OS {
        "macos" => "open",
        "linux" => "xdg-open",
        _ => return,
    };
    let _ = Command::new(program).arg(url).spawn();
}

fn prompt(label: &str, secret: bool) -> anyhow::Result<String> {
    let value = if secret && io::stdin().is_terminal() {
        rpassword::prompt_password(label)?
    } else {
        print!("{label}");
        io::stdout().flush()?;
        let mut value = String::new();
        io::stdin().read_line(&mut value)?;
        value
    };
    let value = value.trim().to_owned();
    if value.is_empty() {
        bail!("value is required")
    } else {
        Ok(value)
    }
}

fn shared_key(kind: &str, label: &str) -> String {
    format!(
        "{}\0{}",
        kind.trim().to_ascii_lowercase(),
        label.trim().to_ascii_lowercase()
    )
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

fn flag_present(args: &[String], name: &str) -> bool {
    args.iter()
        .any(|value| value == name || value == &format!("{name}=true"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn team_matching_requires_permission_and_rejects_ambiguity() {
        let teams = vec![
            Team {
                id: "1".into(),
                name: "Alpha".into(),
                personal: false,
                use_permission: true,
                manage_accounts: true,
            },
            Team {
                id: "2".into(),
                name: "Alpine".into(),
                personal: false,
                use_permission: true,
                manage_accounts: true,
            },
            Team {
                id: "3".into(),
                name: "Blocked".into(),
                personal: false,
                use_permission: false,
                manage_accounts: false,
            },
        ];
        assert_eq!(match_team(&teams, "Alpha").unwrap().id, "1");
        assert!(match_team(&teams, "Al").is_err());
        assert!(match_team(&teams, "3").is_err());
    }

    #[test]
    fn account_selector_prefers_exact_kind_label() {
        let uploads = vec![
            LocalUpload {
                kind: "codex".into(),
                label: "work".into(),
                body: Map::new(),
            },
            LocalUpload {
                kind: "claude".into(),
                label: "work".into(),
                body: Map::new(),
            },
        ];
        assert_eq!(
            select_upload(uploads, "codex:work").unwrap()[0].kind,
            "codex"
        );
    }
}
