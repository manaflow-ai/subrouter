//! Cooperative periodic selection of the best active Codex OAuth account.

use std::fs::{self, OpenOptions};
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, bail};
use tracing::{info, warn};

use crate::account::{AuthMode, Provider};
use crate::accounts::CodexStore;
use crate::agents::{opencode, pi};
use crate::proxy::AccountRef;
use crate::selectacct::{LimitWindow, Scheduler, SchedulerRef, score_from_limit_windows};
use crate::session;

pub fn spawn(
    interval: Duration,
    accounts: AccountRef,
    store: CodexStore,
    sessions: Arc<session::Store>,
    scheduler: Arc<SchedulerRef>,
    state_dir: PathBuf,
) {
    if interval.is_zero() {
        return;
    }
    tokio::spawn(async move {
        let lease = Lease::new(state_dir.join("sr-auto-switch.lease"));
        let mut timer = tokio::time::interval(interval);
        timer.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            timer.tick().await;
            match lease.acquire(interval) {
                Ok(false) => continue,
                Err(error) => warn!(%error, "sr auto-switch lease unavailable, sweeping anyway"),
                Ok(true) => {}
            }
            match switch_once(&accounts, &store, &sessions, &scheduler).await {
                Ok(account) => info!(%account, "sr auto-switch selected account"),
                Err(error) => warn!(%error, "sr auto-switch failed"),
            }
        }
    });
}

async fn switch_once(
    accounts: &AccountRef,
    store: &CodexStore,
    sessions: &session::Store,
    scheduler_ref: &SchedulerRef,
) -> anyhow::Result<String> {
    let candidates: Vec<_> = accounts
        .all()
        .into_iter()
        .filter(|account| {
            account.provider == Provider::Codex && account.auth_mode == AuthMode::Oauth
        })
        .collect();
    if candidates.is_empty() {
        bail!("no OAuth Codex accounts available for sr auto-switch");
    }
    let statuses = accounts.usage_statuses().await;
    let mut successful = 0usize;
    let scores = statuses
        .into_iter()
        .filter(|status| {
            status.account.provider == Provider::Codex
                && status.account.auth_mode == AuthMode::Oauth
                && status.account.error.is_empty()
                && status.fresh
        })
        .map(|status| {
            successful += 1;
            let windows = status
                .windows
                .iter()
                .map(|window| LimitWindow {
                    name: window.name.clone(),
                    used_percent: window.used_percent,
                    limit_window_seconds: window.limit_window_seconds,
                    reset_after_seconds: window.reset_after_seconds,
                    feature: window.feature.clone(),
                })
                .collect::<Vec<_>>();
            let mut score = score_from_limit_windows(&status.account.id, 0, &windows);
            score.provider = Provider::Codex;
            score.fresh = true;
            score
        })
        .collect::<Vec<_>>();
    if successful == 0 {
        bail!("no fresh OAuth usage scores available");
    }
    let scheduler = Scheduler::new(scores);
    scheduler_ref.set(scheduler.clone());
    let scheduler = scheduler.with_session_counts(sessions.count_by_account());
    let account = scheduler.pick(&candidates)?;
    if scheduler.exhausted(account.provider, &account.id) {
        bail!("no usable OAuth Codex accounts available");
    }
    let selected = store.switch_active(&account.id)?;
    let stored = store
        .find_stored(&selected.id)?
        .ok_or_else(|| anyhow!("selected account disappeared"))?;
    if let Err(error) = opencode::sync_codex_account(&stored) {
        warn!(%error, "sr auto-switch OpenCode auth sync failed");
    }
    if let Err(error) = pi::sync_codex_account(&stored) {
        warn!(%error, "sr auto-switch pi auth sync failed");
    }
    Ok(selected.id)
}

struct Lease {
    path: PathBuf,
}

impl Lease {
    fn new(path: PathBuf) -> Self {
        Self { path }
    }

    fn acquire(&self, interval: Duration) -> anyhow::Result<bool> {
        if interval.is_zero() {
            return Ok(true);
        }
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)?;
        }
        let lock_path = self.path.with_extension("lease.lock");
        let Some(_lock) = Lock::acquire(&lock_path)? else {
            return Ok(false);
        };
        if let Ok(metadata) = fs::metadata(&self.path) {
            let tolerance = interval - interval / 10;
            if metadata
                .modified()
                .ok()
                .and_then(|modified| modified.elapsed().ok())
                .is_some_and(|elapsed| elapsed < tolerance)
            {
                return Ok(false);
            }
        }
        let file = OpenOptions::new()
            .create(true)
            .truncate(true)
            .write(true)
            .open(&self.path)?;
        set_private(&self.path)?;
        file.sync_all()?;
        Ok(true)
    }
}

struct Lock(PathBuf);

impl Lock {
    fn acquire(path: &Path) -> anyhow::Result<Option<Self>> {
        match OpenOptions::new().create_new(true).write(true).open(path) {
            Ok(file) => {
                set_private(path)?;
                file.sync_all()?;
                Ok(Some(Self(path.to_path_buf())))
            }
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
                let stale = fs::metadata(path)
                    .and_then(|metadata| metadata.modified())
                    .ok()
                    .and_then(|modified| modified.elapsed().ok())
                    .is_some_and(|elapsed| elapsed > Duration::from_secs(120));
                if stale {
                    match fs::remove_file(path) {
                        Ok(()) => return Self::acquire(path),
                        Err(error) if error.kind() == io::ErrorKind::NotFound => {
                            return Self::acquire(path);
                        }
                        Err(_) => {}
                    }
                }
                Ok(None)
            }
            Err(error) => Err(error.into()),
        }
    }
}

impl Drop for Lock {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.0);
    }
}

#[cfg(unix)]
fn set_private(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt as _;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))
}

#[cfg(not(unix))]
const fn set_private(_path: &Path) -> io::Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lease_allows_one_worker_per_interval_and_reclaims_stale_lock() {
        let temp = tempfile::tempdir().unwrap();
        let lease = Lease::new(temp.path().join("auto.lease"));
        assert!(lease.acquire(Duration::from_secs(60)).unwrap());
        assert!(!lease.acquire(Duration::from_secs(60)).unwrap());
        let lock = lease.path.with_extension("lease.lock");
        fs::write(&lock, b"").unwrap();
        assert!(!lease.acquire(Duration::from_secs(60)).unwrap());
    }
}
