//! Native service installers and lifecycle control.

use std::env;
use std::fs;
use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use clap::{ArgAction, Parser};

const DAEMON_LABEL: &str = "ai.manaflow.subrouter";
const SYSTEMD_SERVICE: &str = "subrouter";
const WINDOWS_TASK: &str = r"\Subrouter\Daemon";

#[derive(Parser)]
#[command(name = "subrouter install-daemon")]
struct LaunchdOptions {
    #[arg(long, default_value = DAEMON_LABEL)]
    label: String,
    #[arg(long, default_value = "127.0.0.1:31415")]
    addr: SocketAddr,
    #[arg(long)]
    install_path: Option<PathBuf>,
    #[arg(long)]
    transcripts: Option<PathBuf>,
    #[arg(long)]
    log_dir: Option<PathBuf>,
    #[arg(long)]
    working_directory: Option<PathBuf>,
    #[arg(long, alias = "cx-switch-interval", default_value = "10m")]
    sr_switch_interval: String,
    #[arg(long)]
    path: Option<String>,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    install_sr_shim: bool,
    #[arg(long)]
    sr_shim_path: Option<PathBuf>,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    install_cx_shim: bool,
    #[arg(long)]
    cx_shim_path: Option<PathBuf>,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    start: bool,
    #[arg(long)]
    dry_run: bool,
}

#[derive(Parser)]
#[command(name = "subrouter install-systemd")]
struct SystemdOptions {
    #[arg(long, default_value = SYSTEMD_SERVICE)]
    service: String,
    #[arg(long, default_value = "subrouter")]
    user: String,
    #[arg(long, default_value = "subrouter")]
    group: String,
    #[arg(long, default_value = "/var/lib/subrouter")]
    home: PathBuf,
    #[arg(long, default_value = "0.0.0.0:31415")]
    addr: SocketAddr,
    #[arg(long, default_value = "/usr/local/bin/subrouter")]
    install_path: PathBuf,
    #[arg(long, default_value = "/var/lib/subrouter/sessions.json")]
    sessions: PathBuf,
    #[arg(long)]
    transcripts: Option<PathBuf>,
    #[arg(long, alias = "cx-switch-interval", default_value = "10m")]
    sr_switch_interval: String,
    #[arg(long, env = "SUBROUTER_ADMIN_TOKEN", hide_env_values = true)]
    admin_token: Option<String>,
    #[arg(long, default_value = "")]
    extra_args: String,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    start: bool,
    #[arg(long)]
    dry_run: bool,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    install_aliases: bool,
}

#[derive(Parser)]
#[command(name = "subrouter install-windows-task")]
struct WindowsOptions {
    #[arg(long, default_value = WINDOWS_TASK)]
    task_name: String,
    #[arg(long, default_value = "127.0.0.1:31415")]
    addr: SocketAddr,
    #[arg(long)]
    install_path: Option<PathBuf>,
    #[arg(long, alias = "cx-switch-interval", default_value = "10m")]
    sr_switch_interval: String,
    #[arg(long, default_value_t = true, action = ArgAction::Set)]
    start: bool,
    #[arg(long)]
    dry_run: bool,
}

pub fn install_launchd(args: &[String]) -> anyhow::Result<()> {
    let options = LaunchdOptions::try_parse_from(
        std::iter::once("subrouter install-daemon").chain(args.iter().map(String::as_str)),
    )?;
    let home = home_dir()?;
    if !options.addr.ip().is_loopback() {
        bail!("install-daemon only supports loopback addresses");
    }
    humantime::parse_duration(&options.sr_switch_interval).context("--sr-switch-interval")?;
    let install_path = options
        .install_path
        .unwrap_or_else(|| home.join("bin/subrouter"));
    let sr_path = options.sr_shim_path.unwrap_or_else(|| home.join("bin/sr"));
    let cx_path = options.cx_shim_path.unwrap_or_else(|| home.join("bin/cx"));
    let log_dir = options.log_dir.unwrap_or_else(|| home.join("Library/Logs"));
    let working_directory = options.working_directory.unwrap_or(env::current_dir()?);
    let daemon_path = options.path.unwrap_or_else(|| {
        format!(
            "{}:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
            install_path
                .parent()
                .unwrap_or_else(|| Path::new("/usr/local/bin"))
                .display()
        )
    });
    let cloud_config = cloud_config_path(&home);
    let plist = launchd_plist(
        &options.label,
        options.addr,
        &install_path,
        options.transcripts.as_deref(),
        &log_dir,
        &working_directory,
        &options.sr_switch_interval,
        &daemon_path,
        &home,
        &cloud_config,
    );
    if options.dry_run {
        print!("{plist}");
        return Ok(());
    }
    if !cfg!(target_os = "macos") {
        bail!("install-daemon is macOS-only; use install-systemd on Linux")
    }
    fs::create_dir_all(
        install_path
            .parent()
            .ok_or_else(|| anyhow!("install path has no parent"))?,
    )?;
    fs::create_dir_all(&log_dir)?;
    if let Some(dir) = &options.transcripts {
        create_private_dir(dir)?;
    }
    let agents_dir = home.join("Library/LaunchAgents");
    fs::create_dir_all(&agents_dir)?;
    install_current_executable(&install_path)?;
    if options.install_sr_shim {
        install_alias(&install_path, &sr_path)?;
    }
    if options.install_cx_shim {
        install_alias(&install_path, &cx_path)?;
    }
    let plist_path = agents_dir.join(format!("{}.plist", options.label));
    fs::write(&plist_path, plist)?;
    if options.start {
        restart_launchd(&options.label, &plist_path)?;
    }
    println!("Installed {}", install_path.display());
    println!("Installed {}", plist_path.display());
    if options.start {
        println!("Started {}", options.label);
    }
    Ok(())
}

pub fn install_systemd(args: &[String]) -> anyhow::Result<()> {
    let mut options = SystemdOptions::try_parse_from(
        std::iter::once("subrouter install-systemd").chain(args.iter().map(String::as_str)),
    )?;
    validate_unit_name(&options.service)?;
    humantime::parse_duration(&options.sr_switch_interval).context("--sr-switch-interval")?;
    let defaults_path = PathBuf::from(format!("/etc/default/{}", options.service));
    preserve_systemd_defaults(&mut options, &defaults_path);
    let unit = systemd_service(&options);
    let socket = systemd_socket(&options);
    if options.dry_run {
        print!("{unit}\n{socket}");
        return Ok(());
    }
    if !cfg!(target_os = "linux") {
        bail!("install-systemd is Linux-only")
    }
    #[cfg(unix)]
    if !nix::unistd::geteuid().is_root() {
        bail!("install-systemd must run as root; use sudo")
    }
    ensure_system_identity(&options.user, &options.group, &options.home)?;
    for dir in [
        options.home.clone(),
        options.home.join(".codex"),
        options.home.join("codex/accounts"),
        options
            .sessions
            .parent()
            .unwrap_or(&options.home)
            .to_path_buf(),
        PathBuf::from("/var/log/subrouter"),
    ] {
        fs::create_dir_all(dir)?;
    }
    if let Some(dir) = &options.transcripts {
        fs::create_dir_all(dir)?;
    }
    install_current_executable(&options.install_path)?;
    if options.install_aliases {
        let parent = options
            .install_path
            .parent()
            .ok_or_else(|| anyhow!("install path has no parent"))?;
        install_alias(&options.install_path, &parent.join("sr"))?;
        install_alias(&options.install_path, &parent.join("cx"))?;
    }
    fs::write(&defaults_path, systemd_defaults(&options))?;
    let unit_path = PathBuf::from(format!("/etc/systemd/system/{}.service", options.service));
    let socket_path = PathBuf::from(format!("/etc/systemd/system/{}.socket", options.service));
    fs::write(&unit_path, unit)?;
    fs::write(&socket_path, socket)?;
    let mut owned = vec![
        options.home.clone(),
        options
            .sessions
            .parent()
            .unwrap_or(&options.home)
            .to_path_buf(),
        PathBuf::from("/var/log/subrouter"),
    ];
    if let Some(dir) = options.transcripts {
        owned.push(dir);
    }
    run(
        "chown",
        std::iter::once(format!("{}:{}", options.user, options.group))
            .chain(owned.iter().map(|path| path.to_string_lossy().into_owned())),
    )?;
    run("systemctl", ["daemon-reload"])?;
    if options.start {
        run(
            "systemctl",
            [
                "enable",
                &format!("{}.socket", options.service),
                &options.service,
            ],
        )?;
        run("systemctl", ["restart", &options.service])?;
    }
    println!("Installed {}", options.install_path.display());
    println!("Installed {}", unit_path.display());
    println!("Installed {}", socket_path.display());
    Ok(())
}

pub fn install_user_systemd() -> anyhow::Result<()> {
    if !cfg!(target_os = "linux") {
        bail!("per-user systemd installation is Linux-only")
    }
    let home = home_dir()?;
    for conflict in [
        "/etc/systemd/system/subrouter.service",
        "/etc/systemd/system/subrouter.socket",
    ] {
        if Path::new(conflict).exists() {
            bail!(
                "existing system-wide Subrouter service conflicts with the per-user daemon: {conflict}"
            );
        }
    }
    let bin_dir = home.join(".local/bin");
    let install_path = bin_dir.join("subrouter");
    let unit_path = home.join(".config/systemd/user/subrouter.service");
    for dir in [
        &bin_dir,
        &home.join(".config/subrouter"),
        &home.join(".subrouter"),
        unit_path.parent().unwrap(),
    ] {
        create_private_dir(dir)?;
    }
    install_current_executable(&install_path)?;
    install_alias(&install_path, &bin_dir.join("sr"))?;
    install_alias(&install_path, &bin_dir.join("cx"))?;
    fs::write(&unit_path, user_systemd_service(&home, &install_path))?;
    run("systemctl", ["--user", "daemon-reload"])?;
    run("systemctl", ["--user", "enable", "--now", SYSTEMD_SERVICE])?;
    println!("Installed {}", install_path.display());
    println!("Installed {}", unit_path.display());
    println!("Started subrouter.service (user)");
    Ok(())
}

pub fn install_windows_task(args: &[String]) -> anyhow::Result<()> {
    let options = WindowsOptions::try_parse_from(
        std::iter::once("subrouter install-windows-task").chain(args.iter().map(String::as_str)),
    )?;
    if !options.addr.ip().is_loopback() {
        bail!("the Windows daemon must listen on loopback")
    }
    humantime::parse_duration(&options.sr_switch_interval).context("--sr-switch-interval")?;
    let local = env::var_os("LOCALAPPDATA")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(r"C:\Users\User\AppData\Local"));
    let root = local.join("Subrouter");
    let executable = options
        .install_path
        .unwrap_or_else(|| root.join("bin/subrouter.exe"));
    let xml = windows_task_xml(
        &options.task_name,
        &executable,
        &root,
        options.addr,
        &options.sr_switch_interval,
    );
    if options.dry_run {
        print!("{xml}");
        return Ok(());
    }
    if !cfg!(windows) {
        bail!("install-windows-task is Windows-only")
    }
    let task_dir = root.join("tasks");
    for dir in [
        root.join("bin"),
        root.join("logs"),
        root.join("state"),
        root.join("config"),
        task_dir.clone(),
    ] {
        fs::create_dir_all(dir)?;
    }
    install_current_executable(&executable)?;
    let xml_path = task_dir.join("daemon.xml");
    fs::write(&xml_path, utf16le(&xml))?;
    run(
        "schtasks",
        [
            "/Create",
            "/TN",
            &options.task_name,
            "/XML",
            &xml_path.to_string_lossy(),
            "/F",
        ],
    )?;
    if options.start {
        run("schtasks", ["/Run", "/TN", &options.task_name])?;
    }
    println!("Installed {}", executable.display());
    println!("Registered {}", options.task_name);
    Ok(())
}

pub async fn daemon_command(args: &[String]) -> anyhow::Result<()> {
    let action = args.first().map(String::as_str).unwrap_or("status");
    match action {
        "status" => daemon_status().await,
        "start" | "up" => lifecycle("start").await,
        "stop" | "down" => lifecycle("stop").await,
        "restart" => lifecycle("restart").await,
        "logs" => follow_logs().await,
        "help" | "-h" | "--help" => {
            println!("Usage: sr daemon <start|stop|restart|status|logs>");
            Ok(())
        }
        value => bail!("unknown daemon command {value:?}"),
    }
}

#[must_use]
pub fn daemon_installed() -> bool {
    match env::consts::OS {
        "macos" => home_dir().is_ok_and(|home| {
            home.join(format!("Library/LaunchAgents/{DAEMON_LABEL}.plist"))
                .exists()
        }),
        "linux" => {
            Path::new("/etc/systemd/system/subrouter.service").exists()
                || home_dir()
                    .is_ok_and(|home| home.join(".config/systemd/user/subrouter.service").exists())
        }
        "windows" => Command::new("schtasks")
            .args(["/Query", "/TN", WINDOWS_TASK])
            .status()
            .is_ok_and(|status| status.success()),
        _ => false,
    }
}

pub async fn install_for_current_user() -> anyhow::Result<()> {
    match env::consts::OS {
        "macos" => install_launchd(&[]),
        "linux" => install_user_systemd(),
        "windows" => install_windows_task(&[]),
        os => bail!("daemon installation is unsupported on {os}"),
    }
}

pub async fn restart_if_installed() -> anyhow::Result<()> {
    if daemon_installed() {
        lifecycle("restart").await?;
    }
    Ok(())
}

pub fn remove_installed() -> anyhow::Result<bool> {
    if !daemon_installed() {
        return Ok(false);
    }
    match env::consts::OS {
        "macos" => {
            let home = home_dir()?;
            let plist = home.join(format!("Library/LaunchAgents/{DAEMON_LABEL}.plist"));
            let domain = format!("gui/{}", current_uid());
            let _ = run("launchctl", ["bootout", &domain, &plist.to_string_lossy()]);
            fs::remove_file(plist)?;
        }
        "linux" => {
            let user_unit = home_dir()?.join(".config/systemd/user/subrouter.service");
            if user_unit.exists() {
                let _ = run("systemctl", ["--user", "disable", "--now", SYSTEMD_SERVICE]);
                fs::remove_file(user_unit)?;
                run("systemctl", ["--user", "daemon-reload"])?;
            } else {
                let _ = run("systemctl", ["disable", "--now", SYSTEMD_SERVICE]);
                for path in [
                    "/etc/systemd/system/subrouter.service",
                    "/etc/systemd/system/subrouter.socket",
                ] {
                    match fs::remove_file(path) {
                        Ok(()) => {}
                        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                        Err(error) => return Err(error.into()),
                    }
                }
                run("systemctl", ["daemon-reload"])?;
            }
        }
        "windows" => {
            let _ = run("schtasks", ["/End", "/TN", WINDOWS_TASK]);
            run("schtasks", ["/Delete", "/TN", WINDOWS_TASK, "/F"])?;
        }
        os => bail!("daemon removal is unsupported on {os}"),
    }
    Ok(true)
}

async fn lifecycle(action: &str) -> anyhow::Result<()> {
    match env::consts::OS {
        "macos" => {
            let home = home_dir()?;
            let plist = home.join(format!("Library/LaunchAgents/{DAEMON_LABEL}.plist"));
            if !plist.exists() {
                bail!("no local Subrouter daemon installed; run 'sr setup' first")
            }
            let domain = format!("gui/{}", current_uid());
            match action {
                "start" => match run(
                    "launchctl",
                    ["bootstrap", &domain, &plist.to_string_lossy()],
                ) {
                    Ok(()) => {}
                    Err(_) => run(
                        "launchctl",
                        ["kickstart", "-k", &format!("{domain}/{DAEMON_LABEL}")],
                    )?,
                },
                "stop" => run("launchctl", ["bootout", &domain, &plist.to_string_lossy()])?,
                "restart" => restart_launchd(DAEMON_LABEL, &plist)?,
                _ => unreachable!(),
            }
        }
        "linux" => {
            let user_unit = home_dir()?.join(".config/systemd/user/subrouter.service");
            if user_unit.exists() {
                run("systemctl", ["--user", action, SYSTEMD_SERVICE])?;
            } else if Path::new("/etc/systemd/system/subrouter.service").exists() {
                run("systemctl", [action, SYSTEMD_SERVICE])?;
            } else {
                bail!("no local Subrouter daemon installed; run 'sr setup' first")
            }
        }
        "windows" => {
            let verb = match action {
                "start" => "/Run",
                "stop" => "/End",
                "restart" => "/Run",
                _ => unreachable!(),
            };
            if action == "restart" {
                let _ = run("schtasks", ["/End", "/TN", WINDOWS_TASK]);
            }
            run("schtasks", [verb, "/TN", WINDOWS_TASK])?;
        }
        os => bail!("daemon management is unsupported on {os}"),
    }
    if action != "stop" && !wait_for_health(Duration::from_secs(15)).await {
        bail!("daemon did not become healthy")
    }
    let completed = match action {
        "start" => "Started",
        "stop" => "Stopped",
        "restart" => "Restarted",
        _ => unreachable!(),
    };
    println!("{completed} Subrouter daemon");
    Ok(())
}

async fn daemon_status() -> anyhow::Result<()> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .build()?;
    match client
        .get("http://127.0.0.1:31415/_subrouter/health")
        .send()
        .await
    {
        Ok(response) if response.status().is_success() => {
            println!("http://127.0.0.1:31415        healthy")
        }
        _ => println!("http://127.0.0.1:31415        UNREACHABLE"),
    }
    Ok(())
}

async fn wait_for_health(timeout: Duration) -> bool {
    let deadline = tokio::time::Instant::now() + timeout;
    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(1))
        .build()
    {
        Ok(client) => client,
        Err(_) => return false,
    };
    while tokio::time::Instant::now() < deadline {
        if client
            .get("http://127.0.0.1:31415/_subrouter/health")
            .send()
            .await
            .is_ok_and(|response| response.status().is_success())
        {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    false
}

async fn follow_logs() -> anyhow::Result<()> {
    let mut command = match env::consts::OS {
        "macos" => {
            let home = home_dir()?;
            let mut command = tokio::process::Command::new("tail");
            command
                .args(["-n", "100", "-F"])
                .arg(home.join("Library/Logs/subrouter.log"))
                .arg(home.join("Library/Logs/subrouter.err.log"));
            command
        }
        "linux" => {
            let mut command = tokio::process::Command::new("journalctl");
            if home_dir()?
                .join(".config/systemd/user/subrouter.service")
                .exists()
            {
                command.arg("--user");
            }
            command.args(["--unit", SYSTEMD_SERVICE, "--lines", "100", "--follow"]);
            command
        }
        os => bail!("daemon logs are unsupported on {os}"),
    };
    let status = command
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .await?;
    if !status.success() {
        bail!("log command exited with {status}")
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn launchd_plist(
    label: &str,
    addr: SocketAddr,
    executable: &Path,
    transcripts: Option<&Path>,
    log_dir: &Path,
    working_directory: &Path,
    interval: &str,
    path: &str,
    home: &Path,
    cloud_config: &Path,
) -> String {
    let transcript_args = transcripts.map_or_else(String::new, |dir| {
        format!(
            "\n        <string>--transcripts</string>\n        <string>{}</string>",
            xml(&dir.to_string_lossy())
        )
    });
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>EnvironmentVariables</key><dict><key>HOME</key><string>{home}</string><key>PATH</key><string>{path}</string></dict>
    <key>KeepAlive</key><true/>
    <key>Label</key><string>{label}</string>
    <key>ProgramArguments</key><array>
        <string>{executable}</string><string>serve</string><string>--addr</string><string>{addr}</string>
        <string>--cloud-config</string><string>{cloud_config}</string>{transcript_args}
        <string>--sr-switch-interval</string><string>{interval}</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>StandardErrorPath</key><string>{error_log}</string>
    <key>StandardOutPath</key><string>{log}</string>
    <key>WorkingDirectory</key><string>{working_directory}</string>
</dict>
</plist>
"#,
        home = xml(&home.to_string_lossy()),
        path = xml(path),
        label = xml(label),
        executable = xml(&executable.to_string_lossy()),
        addr = xml(&addr.to_string()),
        cloud_config = xml(&cloud_config.to_string_lossy()),
        interval = xml(interval),
        error_log = xml(&log_dir.join("subrouter.err.log").to_string_lossy()),
        log = xml(&log_dir.join("subrouter.log").to_string_lossy()),
        working_directory = xml(&working_directory.to_string_lossy()),
    )
}

fn systemd_service(options: &SystemdOptions) -> String {
    format!(
        "[Unit]\nDescription=Subrouter AI agent router\nWants=network-online.target\nRequires={0}.socket\nAfter=network-online.target {0}.socket\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=simple\nSockets={0}.socket\nUser={1}\nGroup={2}\nWorkingDirectory={3}\nEnvironment=HOME={3}\nEnvironmentFile=-/etc/default/{0}\nExecStart={4} serve --addr ${{SUBROUTER_ADDR}} --sessions ${{SUBROUTER_SESSIONS}} $SUBROUTER_TRANSCRIPT_ARGS --sr-switch-interval ${{SUBROUTER_SR_SWITCH_INTERVAL}} $SUBROUTER_EXTRA_ARGS\nRestart=on-failure\nRestartSec=3\nTimeoutStopSec=10min\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=full\nProtectHome=true\nReadWritePaths={3} /var/log/subrouter\n\n[Install]\nWantedBy=multi-user.target\n",
        options.service,
        options.user,
        options.group,
        options.home.display(),
        options.install_path.display(),
    )
}

fn systemd_socket(options: &SystemdOptions) -> String {
    format!(
        "[Unit]\nDescription=Subrouter AI agent router socket\n\n[Socket]\nListenStream={}\nNoDelay=true\nService={}.service\n\n[Install]\nWantedBy=sockets.target\n",
        options.addr, options.service
    )
}

fn systemd_defaults(options: &SystemdOptions) -> String {
    let transcript = options
        .transcripts
        .as_ref()
        .map_or_else(String::new, |path| path.to_string_lossy().into_owned());
    let transcript_args = if transcript.is_empty() {
        String::new()
    } else {
        format!("--transcripts={transcript}")
    };
    format!(
        "SUBROUTER_ADDR={}\nSUBROUTER_STATE_DIR={}\nSUBROUTER_SESSIONS={}\nSUBROUTER_TRANSCRIPTS={}\nSUBROUTER_TRANSCRIPT_ARGS={}\nSUBROUTER_SR_SWITCH_INTERVAL={}\nSUBROUTER_ADMIN_TOKEN={}\nSUBROUTER_EXTRA_ARGS={}\n",
        options.addr,
        options.home.display(),
        options.sessions.display(),
        transcript,
        shell_quote(&transcript_args),
        shell_quote(&options.sr_switch_interval),
        shell_quote(options.admin_token.as_deref().unwrap_or_default()),
        shell_quote(&options.extra_args),
    )
}

fn preserve_systemd_defaults(options: &mut SystemdOptions, path: &Path) {
    if options.extra_args.is_empty() {
        options.extra_args = default_value(path, "SUBROUTER_EXTRA_ARGS");
    }
    if options.transcripts.is_none() {
        let value = default_value(path, "SUBROUTER_TRANSCRIPTS");
        if !value.is_empty() {
            options.transcripts = Some(value.into());
        }
    }
    if options.admin_token.as_deref().is_none_or(str::is_empty) {
        let value = default_value(path, "SUBROUTER_ADMIN_TOKEN");
        if !value.is_empty() {
            options.admin_token = Some(value);
        }
    }
}

fn default_value(path: &Path, name: &str) -> String {
    fs::read_to_string(path)
        .ok()
        .and_then(|body| {
            body.lines().find_map(|line| {
                let (key, value) = line.split_once('=')?;
                (key.trim() == name).then(|| value.trim().trim_matches(['\'', '"']).to_owned())
            })
        })
        .unwrap_or_default()
}

fn user_systemd_service(home: &Path, executable: &Path) -> String {
    let config = cloud_config_path(home);
    format!(
        "[Unit]\nDescription=Subrouter local credential proxy\nAfter=network-online.target\nWants=network-online.target\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=simple\nEnvironment={}\nExecStart={} serve --addr 127.0.0.1:31415 --cloud-config {} --sr-switch-interval 0\nRestart=on-failure\nRestartSec=3\nTimeoutStopSec=10min\nUMask=0077\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths={} {} -{}\nBindReadOnlyPaths={}\nRestrictSUIDSGID=true\n\n[Install]\nWantedBy=default.target\n",
        systemd_quote(&format!("HOME={}", home.display())),
        systemd_quote(&executable.to_string_lossy()),
        systemd_quote(&config.to_string_lossy()),
        systemd_quote(&home.join(".subrouter").to_string_lossy()),
        systemd_quote(&home.join(".config/subrouter").to_string_lossy()),
        systemd_quote(&home.join(".codex-accounts").to_string_lossy()),
        systemd_quote(&config.to_string_lossy()),
    )
}

fn windows_task_xml(
    task: &str,
    executable: &Path,
    working_directory: &Path,
    addr: SocketAddr,
    interval: &str,
) -> String {
    let _ = task;
    format!(
        r#"<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Author>Subrouter</Author><Description>Subrouter local daemon (loopback only)</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><StartWhenAvailable>true</StartWhenAvailable><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings>
  <Actions Context="Author"><Exec><Command>{}</Command><Arguments>serve --addr {} --sr-switch-interval {}</Arguments><WorkingDirectory>{}</WorkingDirectory></Exec></Actions>
</Task>
"#,
        xml(&executable.to_string_lossy()),
        xml(&addr.to_string()),
        xml(interval),
        xml(&working_directory.to_string_lossy()),
    )
}

fn install_current_executable(destination: &Path) -> anyhow::Result<()> {
    let source = fs::canonicalize(env::current_exe()?)?;
    if destination.exists() && fs::canonicalize(destination).ok().as_ref() == Some(&source) {
        return Ok(());
    }
    let parent = destination
        .parent()
        .ok_or_else(|| anyhow!("install path has no parent"))?;
    fs::create_dir_all(parent)?;
    let mut temp = tempfile::Builder::new()
        .prefix(".subrouter-install-")
        .tempfile_in(parent)?;
    io::copy(&mut fs::File::open(source)?, temp.as_file_mut())?;
    temp.as_file().sync_all()?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        fs::set_permissions(temp.path(), fs::Permissions::from_mode(0o755))?;
    }
    temp.persist(destination).map_err(|error| error.error)?;
    Ok(())
}

fn install_alias(executable: &Path, alias: &Path) -> anyhow::Result<()> {
    if executable == alias {
        return Ok(());
    }
    let parent = alias
        .parent()
        .ok_or_else(|| anyhow!("alias path has no parent"))?;
    fs::create_dir_all(parent)?;
    match fs::remove_file(alias) {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error.into()),
    }
    #[cfg(unix)]
    std::os::unix::fs::symlink(fs::canonicalize(executable)?, alias)?;
    #[cfg(windows)]
    {
        fs::copy(executable, alias)?;
    }
    Ok(())
}

fn restart_launchd(label: &str, plist: &Path) -> anyhow::Result<()> {
    let domain = format!("gui/{}", current_uid());
    let _ = run("launchctl", ["bootout", &domain, &plist.to_string_lossy()]);
    run(
        "launchctl",
        ["bootstrap", &domain, &plist.to_string_lossy()],
    )?;
    run(
        "launchctl",
        ["kickstart", "-k", &format!("{domain}/{label}")],
    )
}

fn ensure_system_identity(user: &str, group: &str, home: &Path) -> anyhow::Result<()> {
    if !Command::new("getent")
        .args(["group", group])
        .status()
        .is_ok_and(|status| status.success())
    {
        run("groupadd", ["--system", group])?;
    }
    if !Command::new("id")
        .args(["-u", user])
        .status()
        .is_ok_and(|status| status.success())
    {
        run(
            "useradd",
            [
                "--system",
                "--gid",
                group,
                "--home-dir",
                &home.to_string_lossy(),
                "--create-home",
                "--shell",
                "/usr/sbin/nologin",
                user,
            ],
        )?;
    }
    Ok(())
}

fn run<I, S>(program: &str, args: I) -> anyhow::Result<()>
where
    I: IntoIterator<Item = S>,
    S: AsRef<std::ffi::OsStr>,
{
    let args: Vec<_> = args
        .into_iter()
        .map(|value| value.as_ref().to_owned())
        .collect();
    let output = Command::new(program)
        .args(&args)
        .output()
        .with_context(|| format!("run {program}"))?;
    if !output.status.success() {
        bail!(
            "{} {}: {}",
            program,
            args.iter()
                .map(|value| value.to_string_lossy())
                .collect::<Vec<_>>()
                .join(" "),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    Ok(())
}

fn home_dir() -> anyhow::Result<PathBuf> {
    env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .map(PathBuf::from)
        .filter(|path| !path.as_os_str().is_empty())
        .ok_or_else(|| anyhow!("home directory is unavailable"))
}

fn cloud_config_path(home: &Path) -> PathBuf {
    env::var_os("SUBROUTER_CLOUD_CONFIG")
        .filter(|value| !value.to_string_lossy().trim().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| home.join(".config/subrouter/cloud.json"))
}

fn create_private_dir(path: &Path) -> anyhow::Result<()> {
    fs::create_dir_all(path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

fn validate_unit_name(value: &str) -> anyhow::Result<()> {
    if value.trim().is_empty() || value.contains(['/', '\\']) {
        bail!("service must be a systemd unit basename")
    }
    Ok(())
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

fn systemd_quote(value: &str) -> String {
    format!(
        "\"{}\"",
        value
            .replace('\\', "\\\\")
            .replace('"', "\\\"")
            .replace('%', "%%")
            .replace('\n', "\\n")
    )
}

fn xml(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

fn utf16le(value: &str) -> Vec<u8> {
    let mut output = vec![0xff, 0xfe];
    for unit in value.encode_utf16() {
        output.extend_from_slice(&unit.to_le_bytes());
    }
    output
}

#[cfg(unix)]
fn current_uid() -> u32 {
    nix::unistd::getuid().as_raw()
}

#[cfg(not(unix))]
const fn current_uid() -> u32 {
    0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn launchd_and_systemd_render_without_secrets_in_argv() {
        let home = Path::new("/Users/example");
        let plist = launchd_plist(
            DAEMON_LABEL,
            "127.0.0.1:31415".parse().unwrap(),
            Path::new("/Users/example/bin/subrouter"),
            None,
            Path::new("/Users/example/Library/Logs"),
            home,
            "10m",
            "/usr/bin:/bin",
            home,
            Path::new("/Users/example/.config/subrouter/cloud.json"),
        );
        assert!(plist.contains("<string>serve</string>"));
        assert!(plist.contains("127.0.0.1:31415"));
        let options =
            SystemdOptions::try_parse_from(["install", "--admin-token", "secret", "--dry-run"])
                .unwrap();
        assert!(!systemd_service(&options).contains("secret"));
        assert!(systemd_defaults(&options).contains("secret"));
    }

    #[test]
    fn windows_task_is_utf16_and_loopback_scoped() {
        let xml = windows_task_xml(
            WINDOWS_TASK,
            Path::new(r"C:\Subrouter\subrouter.exe"),
            Path::new(r"C:\Subrouter"),
            "127.0.0.1:31415".parse().unwrap(),
            "10m",
        );
        assert!(xml.contains("serve --addr 127.0.0.1:31415"));
        let encoded = utf16le(&xml);
        assert_eq!(&encoded[..2], &[0xff, 0xfe]);
    }
}
