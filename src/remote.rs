//! Legacy remote-server installation and credential handoff workflows.

use std::collections::{BTreeMap, BTreeSet};
use std::env;
use std::ffi::OsString;
use std::fs;
use std::io::{self, IsTerminal as _, Write as _};
use std::path::Path;
use std::process::Stdio;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use chrono::{SecondsFormat, Utc};
use flate2::Compression;
use flate2::write::GzEncoder;
use reqwest::Client;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use tar::{Builder as TarBuilder, Header};
use tokio::io::AsyncWriteExt as _;
use tokio::process::Command;
use url::Url;

use crate::account::{AuthMode, Provider};
use crate::accounts::{self, CodexAuthFile, CodexStore, StoredCodexAccount};
use crate::agents::claude;
use crate::servers::ServerConfig;

const PUBLIC_INSTALL_SCRIPT_URL: &str =
    "https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh";

pub async fn install_server(server: &ServerConfig, args: &[String]) -> anyhow::Result<()> {
    if server.gcp_instance.is_empty() || server.gcp_zone.is_empty() {
        bail!("server {} has no GCP target", server.name);
    }
    let version = flag_value(args, "--version").unwrap_or_else(|| "latest".into());
    let addr = flag_value(args, "--addr").unwrap_or_else(|| "0.0.0.0:31415".into());
    let interval = flag_value(args, "--sr-switch-interval")
        .or_else(|| flag_value(args, "--cx-switch-interval"))
        .unwrap_or_else(|| "10m".into());
    humantime::parse_duration(&interval).context("--sr-switch-interval")?;
    let extra_args = flag_value(args, "--extra-args").unwrap_or_default();
    let hostname =
        flag_value(args, "--tailscale-hostname").unwrap_or_else(|| server.gcp_instance.clone());
    reject_unknown_flags(
        args,
        &[
            "--version",
            "--addr",
            "--sr-switch-interval",
            "--cx-switch-interval",
            "--extra-args",
            "--tailscale-hostname",
        ],
    )?;
    let tailscale_key = env::var("TAILSCALE_AUTH_KEY")
        .unwrap_or_default()
        .trim()
        .to_owned();
    let remote = [
        "set -eu".into(),
        "tailscale_auth_key=''".into(),
        "read -r tailscale_auth_key || true".into(),
        format!(
            "curl -fsSL {} | sudo env SUBROUTER_VERSION={} sh",
            shell_quote(PUBLIC_INSTALL_SCRIPT_URL),
            shell_quote(&version)
        ),
        format!(
            "sudo /usr/local/bin/sr install-systemd --addr {} --sr-switch-interval {} --extra-args {}",
            shell_quote(&addr),
            shell_quote(&interval),
            shell_quote(&extra_args)
        ),
        format!(
            "if [ -n \"$tailscale_auth_key\" ]; then sudo tailscale up --auth-key \"$tailscale_auth_key\" --hostname {} --ssh --accept-routes=false --accept-dns=false; fi",
            shell_quote(&hostname)
        ),
        "i=0; until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do i=$((i+1)); if [ \"$i\" -ge 30 ]; then exit 1; fi; sleep 1; done".into(),
        "/usr/local/bin/sr --help >/dev/null".into(),
    ]
    .join("\n");
    let mut command = Command::new("gcloud");
    command.args([
        "compute",
        "ssh",
        &server.gcp_instance,
        "--zone",
        &server.gcp_zone,
        "--command",
        &remote,
    ]);
    if !server.gcp_project.is_empty() {
        command.args(["--project", &server.gcp_project]);
    }
    run_with_input(
        command,
        format!("{tailscale_key}\n").into_bytes(),
        Duration::from_secs(10 * 60),
    )
    .await?;
    println!("Installed Subrouter server: {}", server.name);
    if !tailscale_key.is_empty() {
        println!("Joined Tailscale as: {hostname}");
    }
    Ok(())
}

pub async fn login_server(server: &ServerConfig, args: &[String]) -> anyhow::Result<()> {
    let device_auth = parse_device_auth(args)?;
    login_server_as(server, device_auth, "").await
}

pub async fn sync_server(server: &ServerConfig, args: &[String]) -> anyhow::Result<()> {
    let options = SyncOptions::parse(args)?;
    let local = CodexStore::default()
        .list_stored()?
        .into_iter()
        .filter(|account| !account.is_api_key() && !account.email.trim().is_empty())
        .map(|account| (account.email.to_ascii_lowercase(), account))
        .collect::<BTreeMap<_, _>>();
    let (remote, status_available) = remote_oauth_accounts(server).await?;
    let local_names = local.keys().cloned().collect::<BTreeSet<_>>();
    let remote_names = remote.keys().cloned().collect::<BTreeSet<_>>();
    let missing = local_names
        .difference(&remote_names)
        .cloned()
        .collect::<Vec<_>>();
    let present = local_names
        .intersection(&remote_names)
        .cloned()
        .collect::<Vec<_>>();
    let server_only = remote_names
        .difference(&local_names)
        .cloned()
        .collect::<Vec<_>>();
    let invalid = remote
        .iter()
        .filter(|(_, account)| account.auth_checked && !account.auth_valid)
        .map(|(email, _)| email.clone())
        .collect::<Vec<_>>();
    println!("Server: {} ({})", server.name, server.url);
    if !status_available {
        println!(
            "Account status is unavailable on this server version. Run 'sr server install {}'.",
            server.name
        );
    }
    print_group("Missing on server", &missing);
    print_group("Already on server", &present);
    print_group("Invalid on server", &invalid);
    print_group("Server-only OAuth accounts", &server_only);

    let mut targets = missing.into_iter().chain(invalid).collect::<BTreeSet<_>>();
    if !options.emails.is_empty() {
        targets.clear();
        for email in &options.emails {
            let key = email.trim().to_ascii_lowercase();
            if !local.contains_key(&key) && !remote.contains_key(&key) {
                bail!("{email} is not a local or server OAuth account");
            }
            targets.insert(key);
        }
    } else if options.all {
        targets.extend(local.keys().cloned());
    }
    if options.dry_run {
        print_group(
            "Would reauth on server",
            &targets.into_iter().collect::<Vec<_>>(),
        );
        return Ok(());
    }
    if targets.is_empty() {
        println!("No missing local OAuth accounts to reauth on the server.");
        println!(
            "Use --all or --email <email> to replace an existing server-owned refresh-token chain."
        );
        return Ok(());
    }
    if !options.yes
        && !confirm(&format!(
            "Reauth {} account(s) on server {}? [y/N]: ",
            targets.len(),
            server.name
        ))?
    {
        println!("No changes made.");
        return Ok(());
    }
    println!(
        "Each login creates a fresh server-owned OAuth refresh-token chain. Existing local refresh tokens are not uploaded."
    );
    for email in &targets {
        println!("\nSign in as {email} for server {}.", server.name);
        login_server_as(server, options.device_auth, email).await?;
    }
    println!(
        "\nSynced {} server-owned OAuth account(s) to {}.",
        targets.len(),
        server.name
    );
    Ok(())
}

pub async fn add_remote_api_key(server: &ServerConfig, args: &[String]) -> anyhow::Result<()> {
    let provider = parse_provider(flag_value(args, "--provider").as_deref().unwrap_or("codex"))?;
    reject_unknown_flags(args, &["--provider"])?;
    let label = prompt_line("Label (e.g. work, personal): ")?;
    let key = if io::stdin().is_terminal() {
        rpassword::prompt_password("API key: ")?
    } else {
        prompt_line("API key: ")?
    };
    if label.trim().is_empty() || key.trim().is_empty() {
        bail!("label and API key are required");
    }
    if provider == Provider::Codex && !key.starts_with("sk-") {
        bail!("invalid API key format, expected sk-...");
    }
    let prefix = match provider {
        Provider::Codex => "apikey",
        Provider::Claude => "claude",
        Provider::Kimi => "kimi",
        Provider::Zai => "zai",
    };
    let account = StoredCodexAccount {
        email: format!("{prefix}:{}", label.trim()),
        provider: Some(provider),
        added_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
        auth: CodexAuthFile {
            auth_mode: "apikey".into(),
            openai_api_key: key.trim().into(),
            ..CodexAuthFile::default()
        },
        ..StoredCodexAccount::default()
    };
    upload_codex_account(server, &account).await?;
    println!(
        "Added server API-key account {} to {}",
        account.api_key_label(),
        server.name
    );
    Ok(())
}

pub async fn upload_claude_profile(server: &ServerConfig, name: &str) -> anyhow::Result<()> {
    let store = claude::Store::default();
    let profile = store
        .match_profile(name)?
        .ok_or_else(|| anyhow!("Claude profile {name:?} not found"))?;
    let config_dir = store.claude_config_dir(&profile.name);
    let credential = store
        .read_credential(&config_dir)
        .await?
        .filter(|credential| !credential.access_token.is_empty())
        .ok_or_else(|| {
            anyhow!(
                "Claude profile {:?} has no credential to upload",
                profile.name
            )
        })?;
    let state_subdir = server_state_subdir(server).await?;
    let dir = Path::new(&profile.dir)
        .file_name()
        .and_then(|value| value.to_str())
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            anyhow!(
                "could not determine instance dir for profile {:?}",
                profile.name
            )
        })?;
    let mut credential_body = serde_json::to_vec_pretty(&json!({"claudeAiOauth": credential}))?;
    credential_body.push(b'\n');
    let relative = state_path(
        &state_subdir,
        &format!("codex/claude/{dir}/.credentials.json"),
    );
    let archive = build_archive(&relative, &credential_body)?;
    let host = ssh_host(server)
        .ok_or_else(|| anyhow!("server {} has no SSH-able host in its URL", server.name))?;
    let registry = remote_path(&state_subdir, "codex/claude.json");
    let claude_dir = remote_path(&state_subdir, "codex/claude");
    let remote_archive = format!(
        "/tmp/sr-claude-cred-{}.tgz",
        Utc::now().timestamp_nanos_opt().unwrap_or_default()
    );
    let profile_json = shell_quote(&profile.name);
    let dir_json = shell_quote(dir);
    let created = shell_quote(&Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true));
    let jq = shell_quote(
        ".profiles[$name] = {name: $name, createdAt: (.profiles[$name].createdAt // $created), dir: $dir}",
    );
    let command = [
        "set -euo pipefail".into(),
        format!("cat > {}", shell_quote(&remote_archive)),
        owner_probe(),
        reload_probe(server),
        "command -v jq >/dev/null || { echo 'jq is required on the server for Claude profile registration' >&2; exit 1; }".into(),
        format!("sudo install -d -o \"$sr_owner\" -g \"$sr_group\" -m 0700 {}", shell_quote(&claude_dir)),
        format!("sudo tar -C /var/lib/subrouter -xzf {}", shell_quote(&remote_archive)),
        format!("sudo rm -f {}", shell_quote(&remote_archive)),
        format!("sudo chmod 700 {}", shell_quote(&format!("{claude_dir}/{dir}"))),
        format!("sudo chmod 600 {}", shell_quote(&format!("{claude_dir}/{dir}/.credentials.json"))),
        format!("sudo sh -c {}", shell_quote(&format!("test -s {registry} || printf '{{\"profiles\":{{}}}}' > {registry}"))),
        format!("sudo jq --arg name {profile_json} --arg dir {dir_json} --arg created {created} {jq} {} | sudo tee {} >/dev/null", shell_quote(&registry), shell_quote(&format!("{registry}.new"))),
        format!("sudo mv {} {}", shell_quote(&format!("{registry}.new")), shell_quote(&registry)),
        format!("sudo chown -R \"$sr_owner:$sr_group\" {} {}", shell_quote(&claude_dir), shell_quote(&registry)),
        "curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts >/dev/null".into(),
    ]
    .join(" && ");
    upload_ssh(&host, &command, archive, "Claude profile upload").await?;
    write_claude_proxy_env(&config_dir, &server.proxy_root(), &server.tenant_key)?;
    println!(
        "Uploaded Claude profile {} to server {} and switched local runs to the server pool.",
        profile.name, server.name
    );
    Ok(())
}

pub async fn upload_codex_account(
    server: &ServerConfig,
    account: &StoredCodexAccount,
) -> anyhow::Result<()> {
    let state_subdir = server_state_subdir(server).await?;
    let relative = state_path(
        &state_subdir,
        &format!("codex/accounts/{}", account_filename(&account.email)),
    );
    let body = serde_json::to_vec_pretty(account)?;
    let archive = build_archive(&relative, &body)?;
    if let Some(host) = ssh_host(server) {
        let remote_archive = format!(
            "/tmp/sr-server-auth-{}.tgz",
            Utc::now().timestamp_nanos_opt().unwrap_or_default()
        );
        let command = account_install_command(server, &state_subdir, &remote_archive, true);
        match upload_ssh(&host, &command, archive.clone(), "server account upload").await {
            Ok(()) => return Ok(()),
            Err(error) if server.gcp_instance.is_empty() || server.gcp_zone.is_empty() => {
                return Err(error);
            }
            Err(error) => eprintln!("direct server upload failed, falling back to gcloud: {error}"),
        }
    }
    upload_gcloud(server, &state_subdir, archive).await
}

async fn login_server_as(
    server: &ServerConfig,
    device_auth: bool,
    expected_email: &str,
) -> anyhow::Result<()> {
    let login_lock = accounts::lock_active_codex_auth().context("lock server login")?;
    let login_home = tempfile::tempdir()?;
    let mut command =
        Command::new(env::var_os("SUBROUTER_CODEX_BIN").unwrap_or_else(|| OsString::from("codex")));
    command.arg("login").env("CODEX_HOME", login_home.path());
    if device_auth {
        command.arg("--device-auth");
    }
    println!("Opening Codex OAuth login for server {}...", server.name);
    let status = command
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .await?;
    if !status.success() {
        bail!("Codex login failed with {status}");
    }
    let auth = accounts::read_codex_auth_file(&login_home.path().join("auth.json"))?
        .ok_or_else(|| anyhow!("Codex login did not write OAuth auth"))?;
    let tokens = auth
        .tokens
        .as_ref()
        .filter(|tokens| !tokens.id_token.is_empty())
        .ok_or_else(|| anyhow!("Codex login did not write OAuth auth"))?;
    let email = accounts::extract_email_from_jwt(&tokens.id_token)
        .context("extract email from logged-in auth")?;
    if !expected_email.is_empty() && !email.eq_ignore_ascii_case(expected_email) {
        bail!("logged in as {email}, expected {expected_email}; no account was uploaded");
    }
    let account = StoredCodexAccount {
        email: email.clone(),
        added_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
        auth,
        ..StoredCodexAccount::default()
    };
    drop(login_lock);
    upload_codex_account(server, &account).await?;
    println!("Uploaded {email} to server {}.", server.name);
    println!("Local Codex auth was left unchanged.");
    println!(
        "The new {email} refresh token is stored on {}, not kept as your local active login.",
        server.name
    );
    Ok(())
}

#[derive(Default)]
struct SyncOptions {
    device_auth: bool,
    all: bool,
    dry_run: bool,
    yes: bool,
    emails: Vec<String>,
}

impl SyncOptions {
    fn parse(args: &[String]) -> anyhow::Result<Self> {
        let mut output = Self::default();
        let mut index = 0;
        while index < args.len() {
            match args[index].as_str() {
                "--device-auth" => output.device_auth = true,
                "--all" => output.all = true,
                "--dry-run" => output.dry_run = true,
                "--yes" => output.yes = true,
                "--email" => {
                    index += 1;
                    output.emails.push(
                        args.get(index)
                            .ok_or_else(|| anyhow!("--email requires a value"))?
                            .clone(),
                    );
                }
                value if value.starts_with("--email=") => output
                    .emails
                    .push(value.trim_start_matches("--email=").into()),
                value => bail!("unexpected server sync argument {value:?}"),
            }
            index += 1;
        }
        Ok(output)
    }
}

#[derive(Clone, Default, Deserialize)]
struct RemoteAccountStatus {
    #[serde(default)]
    id: String,
    #[serde(default)]
    email: String,
    #[serde(default)]
    provider: Option<Provider>,
    #[serde(default)]
    auth_mode: Option<AuthMode>,
    #[serde(default)]
    auth_checked: bool,
    #[serde(default)]
    auth_valid: bool,
}

async fn remote_oauth_accounts(
    server: &ServerConfig,
) -> anyhow::Result<(BTreeMap<String, RemoteAccountStatus>, bool)> {
    let client = Client::builder()
        .timeout(Duration::from_secs(120))
        .build()?;
    let mut request = client.get(format!(
        "{}/_subrouter/account-status",
        server.control_base_url()
    ));
    if !server.admin_token.is_empty() {
        request = request.bearer_auth(&server.admin_token);
    }
    let response = request.send().await?;
    let available = !matches!(response.status().as_u16(), 404 | 405);
    let response = if available {
        response
    } else {
        let mut fallback = client.get(format!("{}/_subrouter/accounts", server.control_base_url()));
        if !server.admin_token.is_empty() {
            fallback = fallback.bearer_auth(&server.admin_token);
        }
        fallback.send().await?
    };
    if !response.status().is_success() {
        bail!("server account status failed: {}", response.status());
    }
    let accounts: Vec<RemoteAccountStatus> = response.json().await?;
    let mut output = BTreeMap::new();
    for account in accounts {
        if account.provider.unwrap_or_default() != Provider::Codex
            || account.auth_mode.unwrap_or_default() != AuthMode::Oauth
        {
            continue;
        }
        let email = if account.email.trim().is_empty() {
            account.id.trim()
        } else {
            account.email.trim()
        };
        if !email.is_empty() && !email.to_ascii_lowercase().starts_with("apikey:") {
            output.insert(email.to_ascii_lowercase(), account);
        }
    }
    Ok((output, available))
}

async fn server_state_subdir(server: &ServerConfig) -> anyhow::Result<String> {
    if server.tenant_key.trim().is_empty() {
        return Ok(String::new());
    }
    let client = Client::builder().timeout(Duration::from_secs(15)).build()?;
    let response = client
        .get(format!("{}/_subrouter/whoami", server.control_base_url()))
        .send()
        .await?;
    if !response.status().is_success() {
        bail!(
            "resolve tenant for server {} failed: {}",
            server.name,
            response.status()
        );
    }
    let value: Value = response.json().await?;
    let id = value
        .get("tenant_id")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if id.is_empty()
        || !id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        bail!("server {} returned an invalid tenant id", server.name);
    }
    Ok(format!("tenants/{id}"))
}

fn build_archive(path: &str, body: &[u8]) -> anyhow::Result<Vec<u8>> {
    if path.starts_with('/') || path.split('/').any(|part| matches!(part, "" | "." | "..")) {
        bail!("refusing unsafe archive path {path:?}");
    }
    let mut gzip = GzEncoder::new(Vec::new(), Compression::default());
    {
        let mut archive = TarBuilder::new(&mut gzip);
        let mut header = Header::new_gnu();
        header.set_path(path)?;
        header.set_mode(0o600);
        header.set_size(body.len() as u64);
        header.set_cksum();
        archive.append(&header, body)?;
        archive.finish()?;
    }
    Ok(gzip.finish()?)
}

async fn upload_ssh(
    host: &str,
    remote_command: &str,
    archive: Vec<u8>,
    label: &str,
) -> anyhow::Result<()> {
    let mut last = None;
    for attempt in 1..=3 {
        let mut command = Command::new("ssh");
        command.args([
            "-o",
            "BatchMode=yes",
            "-o",
            "ConnectTimeout=15",
            "-o",
            "LogLevel=ERROR",
            "-o",
            "StrictHostKeyChecking=accept-new",
            host,
            remote_command,
        ]);
        match run_with_input(command, archive.clone(), Duration::from_secs(90)).await {
            Ok(()) => return Ok(()),
            Err(error) => last = Some(error),
        }
        if attempt < 3 {
            eprintln!("{label} failed, retrying ({attempt}/3)");
            tokio::time::sleep(Duration::from_secs(attempt)).await;
        }
    }
    Err(last.unwrap_or_else(|| anyhow!("{label} failed")))
}

async fn upload_gcloud(
    server: &ServerConfig,
    state_subdir: &str,
    archive: Vec<u8>,
) -> anyhow::Result<()> {
    if server.gcp_instance.is_empty() || server.gcp_zone.is_empty() {
        bail!("server {} has no GCP target", server.name);
    }
    let mut file = tempfile::NamedTempFile::new()?;
    file.write_all(&archive)?;
    file.flush()?;
    let remote_archive = format!(
        "/tmp/sr-server-auth-{}.tgz",
        Utc::now().timestamp_nanos_opt().unwrap_or_default()
    );
    let mut scp = Command::new("gcloud");
    scp.args([
        "compute",
        "scp",
        &file.path().to_string_lossy(),
        &format!("{}:{remote_archive}", server.gcp_instance),
        "--zone",
        &server.gcp_zone,
    ]);
    if !server.gcp_project.is_empty() {
        scp.args(["--project", &server.gcp_project]);
    }
    run(scp, Duration::from_secs(90))
        .await
        .context("upload account archive")?;
    let remote_command = account_install_command(server, state_subdir, &remote_archive, false);
    let mut ssh = Command::new("gcloud");
    ssh.args([
        "compute",
        "ssh",
        &server.gcp_instance,
        "--zone",
        &server.gcp_zone,
        "--command",
        &remote_command,
    ]);
    if !server.gcp_project.is_empty() {
        ssh.args(["--project", &server.gcp_project]);
    }
    run(ssh, Duration::from_secs(90))
        .await
        .context("install account on server")
}

fn account_install_command(
    server: &ServerConfig,
    state_subdir: &str,
    archive: &str,
    reads_stdin: bool,
) -> String {
    let accounts = remote_path(state_subdir, "codex/accounts");
    let codex = remote_path(state_subdir, "codex");
    let mut commands = vec!["set -euo pipefail".into()];
    if reads_stdin {
        commands.push(format!("cat > {}", shell_quote(archive)));
    }
    commands.extend([
        owner_probe(),
        reload_probe(server),
        format!(
            "sudo install -d -o \"$sr_owner\" -g \"$sr_group\" -m 0750 {}",
            shell_quote(&accounts)
        ),
        format!(
            "sudo tar -C /var/lib/subrouter -xzf {}",
            shell_quote(archive)
        ),
        format!("sudo find {} -name '._*' -delete", shell_quote(&codex)),
        format!(
            "sudo chown -R \"$sr_owner:$sr_group\" {}",
            shell_quote(&codex)
        ),
        format!("sudo rm -f {}", shell_quote(archive)),
        "curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts >/dev/null".into(),
    ]);
    commands.join(" && ")
}

fn owner_probe() -> String {
    "sr_owner=subrouter; sr_group=subrouter; if [ -e /var/lib/subrouter ]; then sr_owner=$(stat -f '%Su' /var/lib/subrouter 2>/dev/null || stat -c '%U' /var/lib/subrouter); sr_group=$(stat -f '%Sg' /var/lib/subrouter 2>/dev/null || stat -c '%G' /var/lib/subrouter); elif id -u _subrouter >/dev/null 2>&1; then sr_owner=_subrouter; sr_group=_subrouter; fi".into()
}

fn reload_probe(server: &ServerConfig) -> String {
    format!(
        "reload_status=$(curl -sS -o /dev/null -w '%{{http_code}}' http://127.0.0.1:31415/_subrouter/reload-accounts || true); if [ \"$reload_status\" != \"405\" ]; then echo {} >&2; exit 1; fi",
        shell_quote(&format!(
            "Subrouter server is too old for hot account reload; run sr server install {} first.",
            server.name
        ))
    )
}

fn state_path(subdir: &str, rest: &str) -> String {
    if subdir.is_empty() {
        rest.into()
    } else {
        format!("{subdir}/{rest}")
    }
}

fn remote_path(subdir: &str, rest: &str) -> String {
    format!("/var/lib/subrouter/{}", state_path(subdir, rest))
}

fn account_filename(email: &str) -> String {
    let mut name = email
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '.' | '_' | '@' | '-') {
                character
            } else {
                '_'
            }
        })
        .collect::<String>();
    name.push_str(".json");
    name
}

fn ssh_host(server: &ServerConfig) -> Option<String> {
    let host = Url::parse(&server.url).ok()?.host_str()?.trim().to_owned();
    (!host.is_empty() && !matches!(host.as_str(), "localhost" | "127.0.0.1" | "::1"))
        .then_some(host)
}

fn write_claude_proxy_env(
    config_dir: &Path,
    base_url: &str,
    tenant_key: &str,
) -> anyhow::Result<()> {
    let path = config_dir.join("settings.json");
    let mut settings = match fs::read(&path) {
        Ok(body) => serde_json::from_slice::<Map<String, Value>>(&body)
            .with_context(|| format!("parse {}", path.display()))?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => Map::new(),
        Err(error) => return Err(error.into()),
    };
    let env = settings
        .entry("env")
        .or_insert_with(|| Value::Object(Map::new()));
    let env = env
        .as_object_mut()
        .ok_or_else(|| anyhow!("{}.env must be an object", path.display()))?;
    env.insert(
        "ANTHROPIC_BASE_URL".into(),
        base_url.trim_end_matches('/').into(),
    );
    if !tenant_key.is_empty() {
        env.insert("ANTHROPIC_AUTH_TOKEN".into(), tenant_key.into());
    } else if env
        .get("ANTHROPIC_AUTH_TOKEN")
        .and_then(Value::as_str)
        .is_none_or(crate::tenant::valid_key_format)
    {
        env.insert("ANTHROPIC_AUTH_TOKEN".into(), "subrouter".into());
    }
    let mut body = serde_json::to_vec_pretty(&settings)?;
    body.push(b'\n');
    crate::agents::opencode::write_atomic_private(&path, &body)?;
    Ok(())
}

async fn run(mut command: Command, timeout: Duration) -> anyhow::Result<()> {
    command.kill_on_drop(true);
    command
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    let status = tokio::time::timeout(timeout, command.status())
        .await
        .map_err(|_| anyhow!("command timed out"))??;
    if !status.success() {
        bail!("command exited with {status}");
    }
    Ok(())
}

async fn run_with_input(
    mut command: Command,
    input: Vec<u8>,
    timeout: Duration,
) -> anyhow::Result<()> {
    command.kill_on_drop(true);
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    let mut child = command.spawn()?;
    if let Some(mut stdin) = child.stdin.take() {
        stdin.write_all(&input).await?;
    }
    let status = tokio::time::timeout(timeout, child.wait())
        .await
        .map_err(|_| anyhow!("command timed out"))??;
    if !status.success() {
        bail!("command exited with {status}");
    }
    Ok(())
}

fn parse_device_auth(args: &[String]) -> anyhow::Result<bool> {
    let mut device = false;
    for value in args {
        match value.as_str() {
            "--device-auth" => device = true,
            value => bail!("unexpected login argument {value:?}"),
        }
    }
    Ok(device)
}

fn parse_provider(value: &str) -> anyhow::Result<Provider> {
    match value.trim().to_ascii_lowercase().as_str() {
        "codex" | "openai" => Ok(Provider::Codex),
        "claude" | "anthropic" => Ok(Provider::Claude),
        "kimi" | "kimi-for-coding" => Ok(Provider::Kimi),
        "zai" | "glm" => Ok(Provider::Zai),
        value => bail!("unsupported API-key provider {value:?}"),
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

fn reject_unknown_flags(args: &[String], value_flags: &[&str]) -> anyhow::Result<()> {
    let mut index = 0;
    while index < args.len() {
        let value = &args[index];
        if value_flags.iter().any(|flag| value == flag) {
            index += 1;
            if index >= args.len() {
                bail!("{value} requires a value");
            }
        } else if !value_flags
            .iter()
            .any(|flag| value.starts_with(&format!("{flag}=")))
        {
            bail!("unexpected argument {value:?}");
        }
        index += 1;
    }
    Ok(())
}

fn print_group(label: &str, values: &[String]) {
    if values.is_empty() {
        println!("{label}: none");
        return;
    }
    println!("{label}:");
    for value in values {
        println!("  {value}");
    }
}

fn confirm(prompt: &str) -> anyhow::Result<bool> {
    if !io::stdin().is_terminal() {
        bail!("confirmation requires a terminal; rerun with --yes");
    }
    print!("{prompt}");
    io::stdout().flush()?;
    let mut answer = String::new();
    io::stdin().read_line(&mut answer)?;
    Ok(matches!(
        answer.trim().to_ascii_lowercase().as_str(),
        "y" | "yes"
    ))
}

fn prompt_line(prompt: &str) -> anyhow::Result<String> {
    print!("{prompt}");
    io::stdout().flush()?;
    let mut value = String::new();
    io::stdin().read_line(&mut value)?;
    let value = value.trim().to_owned();
    if value.is_empty() {
        bail!("value is required");
    }
    Ok(value)
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn archive_paths_and_shell_values_are_bounded() {
        assert!(build_archive("codex/accounts/a@example.com.json", b"secret").is_ok());
        assert!(build_archive("../escape", b"secret").is_err());
        assert_eq!(shell_quote("a'b"), "'a'\"'\"'b'");
    }

    #[test]
    fn sync_flags_support_repeated_email() {
        let options = SyncOptions::parse(&[
            "--email".into(),
            "a@example.com".into(),
            "--email=b@example.com".into(),
            "--dry-run".into(),
        ])
        .unwrap();
        assert_eq!(options.emails, ["a@example.com", "b@example.com"]);
        assert!(options.dry_run);
    }
}
