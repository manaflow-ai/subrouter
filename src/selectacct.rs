use std::collections::{HashMap, HashSet};
use std::sync::RwLock;
use std::time::{Duration, SystemTime};

use thiserror::Error;

use crate::account::{Account, AuthMode, Provider};

pub const MIN_NEW_SESSION_HEADROOM: f64 = 0.40;
pub const VELOCITY_PROJECTION_MINUTES: f64 = 20.0;
pub const LIVE_DEBIT_PER_REQUEST: f64 = 0.020;
pub const DEFAULT_EXHAUSTED_TTL: Duration = Duration::from_secs(10 * 60);

#[derive(Clone, Debug, Default, PartialEq)]
pub struct LimitWindow {
    pub name: String,
    pub used_percent: f64,
    pub limit_window_seconds: i64,
    pub reset_after_seconds: i64,
    pub feature: String,
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct Score {
    pub account_id: String,
    pub provider: Provider,
    pub headroom: f64,
    pub short_headroom: f64,
    pub short_reset_after_seconds: i64,
    pub expiry_pressure: f64,
    pub sessions: usize,
    pub model_scores: HashMap<String, Score>,
    pub fresh: bool,
}

#[derive(Clone, Debug, Default)]
pub struct Scheduler {
    scores: HashMap<String, Score>,
    session_counts: HashMap<String, usize>,
    live_debits: HashMap<String, usize>,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum SelectError {
    #[error("no accounts available")]
    NoAccounts,
}

#[must_use]
pub fn score_key(provider: Provider, account_id: &str) -> String {
    format!("{}\0{account_id}", provider.as_str())
}

impl Scheduler {
    #[must_use]
    pub fn new(scores: impl IntoIterator<Item = Score>) -> Self {
        Self {
            scores: scores
                .into_iter()
                .map(|score| (score_key(score.provider, &score.account_id), score))
                .collect(),
            ..Self::default()
        }
    }

    #[must_use]
    pub fn with_score(mut self, score: Score) -> Self {
        self.scores
            .insert(score_key(score.provider, &score.account_id), score);
        self
    }

    #[must_use]
    pub fn with_session_counts(mut self, counts: HashMap<String, usize>) -> Self {
        self.session_counts = counts;
        self
    }

    #[must_use]
    pub fn with_live_debits(mut self, debits: HashMap<String, usize>) -> Self {
        self.live_debits = debits;
        self
    }

    #[must_use]
    pub fn for_model(&self, model: &str) -> Self {
        let key = model_key(model);
        if key.is_empty() || !self.has_model_score(&key) {
            return self.clone();
        }
        let mut next = self.clone();
        next.scores = self
            .scores
            .iter()
            .map(|(score_key, score)| {
                let model_score = score
                    .model_scores
                    .get(&key)
                    .cloned()
                    .unwrap_or_else(|| Score {
                        account_id: score.account_id.clone(),
                        provider: score.provider,
                        ..Score::default()
                    });
                (score_key.clone(), model_score)
            })
            .collect();
        next
    }

    #[must_use]
    pub fn has_model_pool(&self, model: &str) -> bool {
        let key = model_key(model);
        !key.is_empty() && self.has_model_score(&key)
    }

    fn has_model_score(&self, key: &str) -> bool {
        self.scores
            .values()
            .any(|score| score.model_scores.contains_key(key))
    }

    pub fn pick(&self, candidates: &[Account]) -> Result<Account, SelectError> {
        let mut sorted = candidates.to_vec();
        if sorted.is_empty() {
            return Err(SelectError::NoAccounts);
        }
        sorted.sort_by(|left_account, right_account| {
            let left = self.score(left_account.provider, &left_account.id);
            let right = self.score(right_account.provider, &right_account.id);
            selection_tier(left_account, &left)
                .cmp(&selection_tier(right_account, &right))
                .then_with(|| {
                    right
                        .usable_for_new_session()
                        .cmp(&left.usable_for_new_session())
                })
                .then_with(|| {
                    if left.usable_for_new_session() && right.usable_for_new_session() {
                        right.expiry_pressure.total_cmp(&left.expiry_pressure)
                    } else {
                        std::cmp::Ordering::Equal
                    }
                })
                .then_with(|| right.headroom.total_cmp(&left.headroom))
                .then_with(|| left.sessions.cmp(&right.sessions))
                .then_with(|| left_account.id.cmp(&right_account.id))
        });
        Ok(sorted.remove(0))
    }

    #[must_use]
    pub fn usable_for_new_session(&self, provider: Provider, account_id: &str) -> bool {
        self.score(provider, account_id).usable_for_new_session()
    }

    #[must_use]
    pub fn exhausted(&self, provider: Provider, account_id: &str) -> bool {
        self.score(provider, account_id).exhausted()
    }

    #[must_use]
    pub fn score_for(&self, provider: Provider, account_id: &str) -> Score {
        self.scores
            .get(&score_key(provider, account_id))
            .cloned()
            .unwrap_or_else(|| Score {
                account_id: account_id.into(),
                provider,
                headroom: 1.0,
                short_headroom: 1.0,
                ..Score::default()
            })
    }

    fn score(&self, provider: Provider, account_id: &str) -> Score {
        let mut score = self.score_for(provider, account_id);
        if let Some(sessions) = self.session_counts.get(account_id) {
            score.sessions = *sessions;
        }
        if let Some(count) = self
            .live_debits
            .get(&score_key(provider, account_id))
            .copied()
            .filter(|count| *count > 0)
        {
            let debit = LIVE_DEBIT_PER_REQUEST * count as f64;
            if score.headroom > 0.01 {
                score.headroom = (score.headroom - debit).max(0.01);
            }
            if score.short_headroom > 0.01 {
                score.short_headroom = (score.short_headroom - debit).max(0.01);
            }
        }
        score
    }
}

impl Score {
    #[must_use]
    pub fn usable_for_new_session(&self) -> bool {
        self.headroom >= MIN_NEW_SESSION_HEADROOM && self.short_headroom >= MIN_NEW_SESSION_HEADROOM
    }

    #[must_use]
    pub fn exhausted(&self) -> bool {
        self.headroom <= 0.0 || self.short_headroom <= 0.0
    }
}

fn selection_tier(account: &Account, score: &Score) -> u8 {
    match (
        account.auth_mode,
        score.usable_for_new_session(),
        score.exhausted(),
    ) {
        (AuthMode::Oauth, true, _) => 0,
        (AuthMode::ApiKey, _, _) => 1,
        (AuthMode::Oauth, false, false) => 2,
        (AuthMode::Oauth, false, true) => 3,
    }
}

#[must_use]
pub fn score_from_limit_windows(
    account_id: &str,
    sessions: usize,
    windows: &[LimitWindow],
) -> Score {
    let mut base = score_from_windows(account_id, sessions, &filter_windows(windows, ""), "");
    for key in distinct_model_keys(windows) {
        base.model_scores.insert(
            key.clone(),
            score_from_windows(account_id, sessions, &filter_windows(windows, &key), &key),
        );
    }
    base
}

#[must_use]
pub fn model_key(model: &str) -> String {
    model
        .chars()
        .filter(char::is_ascii_alphanumeric)
        .map(|character| character.to_ascii_lowercase())
        .collect()
}

fn distinct_model_keys(windows: &[LimitWindow]) -> Vec<String> {
    let mut seen = HashSet::new();
    windows
        .iter()
        .filter_map(|window| {
            let key = model_key(&window.feature);
            (!key.is_empty() && seen.insert(key.clone())).then_some(key)
        })
        .collect()
}

fn filter_windows(windows: &[LimitWindow], key: &str) -> Vec<LimitWindow> {
    windows
        .iter()
        .filter(|window| model_key(&window.feature) == key)
        .cloned()
        .collect()
}

fn score_from_windows(
    account_id: &str,
    sessions: usize,
    windows: &[LimitWindow],
    pool: &str,
) -> Score {
    let mut headroom: f64 = 1.0;
    let mut short_headroom: f64 = 1.0;
    let mut short_reset_after_seconds = 0;
    let mut weekly_pressure: f64 = 0.0;
    let mut has_short = false;
    for window in windows {
        let remaining = 1.0 - window.used_percent.clamp(0.0, 100.0) / 100.0;
        headroom = headroom.min(remaining);
        if window.limit_window_seconds > 0 && window.limit_window_seconds <= 6 * 60 * 60 {
            has_short = true;
            if remaining <= short_headroom {
                short_headroom = remaining;
                short_reset_after_seconds = window.reset_after_seconds;
            }
        }
        if pool == "claudefable"
            && window.limit_window_seconds > 6 * 60 * 60
            && window.reset_after_seconds > 0
        {
            weekly_pressure =
                weekly_pressure.max(expiry_pressure(remaining, window.reset_after_seconds));
        }
    }
    if !has_short {
        short_headroom = headroom;
    }
    let mut pressure = expiry_pressure(headroom, short_reset_after_seconds);
    if pool == "claudefable" {
        pressure += weekly_pressure;
    }
    Score {
        account_id: account_id.into(),
        headroom,
        short_headroom,
        short_reset_after_seconds,
        expiry_pressure: pressure,
        sessions,
        ..Score::default()
    }
}

fn expiry_pressure(headroom: f64, reset_after_seconds: i64) -> f64 {
    if reset_after_seconds <= 0 {
        0.0
    } else {
        headroom / reset_after_seconds as f64
    }
}

#[derive(Debug)]
struct SchedulerState {
    scheduler: Scheduler,
    updated_at: SystemTime,
    refreshing: bool,
    exhausted_until: HashMap<String, SystemTime>,
    incompatible_until: HashMap<String, SystemTime>,
    routed_since_refresh: HashMap<String, usize>,
}

#[derive(Debug)]
pub struct SchedulerRef(RwLock<SchedulerState>);

impl SchedulerRef {
    #[must_use]
    pub fn new(scheduler: Scheduler) -> Self {
        Self(RwLock::new(SchedulerState {
            scheduler,
            updated_at: SystemTime::now(),
            refreshing: false,
            exhausted_until: HashMap::new(),
            incompatible_until: HashMap::new(),
            routed_since_refresh: HashMap::new(),
        }))
    }

    #[must_use]
    pub fn get(&self) -> Scheduler {
        self.prune_expired(SystemTime::now());
        let state = self
            .0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let scheduler =
            apply_exhaustion_marks(&state.scheduler, &state.exhausted_until, SystemTime::now());
        apply_exhaustion_marks(&scheduler, &state.incompatible_until, SystemTime::now())
    }

    pub fn set(&self, scheduler: Scheduler) {
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        update_scheduler(&mut state, scheduler);
        state.updated_at = SystemTime::now();
    }

    pub fn mark_exhausted(&self, provider: Provider, account_id: &str, pool: &str) {
        self.mark_exhausted_until(
            provider,
            account_id,
            pool,
            SystemTime::now() + DEFAULT_EXHAUSTED_TTL,
        );
    }

    pub fn mark_exhausted_until(
        &self,
        provider: Provider,
        account_id: &str,
        pool: &str,
        until: SystemTime,
    ) {
        if account_id.is_empty() {
            return;
        }
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state
            .exhausted_until
            .insert(pool_scoped_key(provider, account_id, pool), until);
        state.updated_at = SystemTime::now();
    }

    pub fn mark_model_incompatible(&self, provider: Provider, account_id: &str, model: &str) {
        self.mark_model_incompatible_until(
            provider,
            account_id,
            model,
            SystemTime::now() + DEFAULT_EXHAUSTED_TTL,
        );
    }

    pub fn mark_model_incompatible_until(
        &self,
        provider: Provider,
        account_id: &str,
        model: &str,
        until: SystemTime,
    ) {
        if account_id.is_empty() || model.is_empty() {
            return;
        }
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state
            .incompatible_until
            .insert(pool_scoped_key(provider, account_id, model), until);
        state.updated_at = SystemTime::now();
    }

    #[must_use]
    pub fn exhausted_until_for(
        &self,
        provider: Provider,
        account_id: &str,
        pool: &str,
    ) -> Option<SystemTime> {
        self.0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .exhausted_until
            .get(&pool_scoped_key(provider, account_id, pool))
            .copied()
    }

    #[must_use]
    pub fn model_incompatible_until_for(
        &self,
        provider: Provider,
        account_id: &str,
        model: &str,
    ) -> Option<SystemTime> {
        self.0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .incompatible_until
            .get(&pool_scoped_key(provider, account_id, model))
            .copied()
    }

    #[must_use]
    pub fn stale(&self, ttl: Duration) -> bool {
        ttl > Duration::ZERO
            && self
                .0
                .read()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .updated_at
                .elapsed()
                .is_ok_and(|elapsed| elapsed >= ttl)
    }

    pub fn begin_refresh_if_stale(&self, ttl: Duration) -> bool {
        if ttl == Duration::ZERO {
            return false;
        }
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if state.refreshing
            || state
                .updated_at
                .elapsed()
                .is_ok_and(|elapsed| elapsed < ttl)
        {
            return false;
        }
        state.refreshing = true;
        true
    }

    pub fn finish_refresh(&self, scheduler: Scheduler, update: bool) {
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if update {
            update_scheduler(&mut state, scheduler);
            state.routed_since_refresh.clear();
        }
        state.updated_at = SystemTime::now();
        state.refreshing = false;
    }

    pub fn note_routed(&self, provider: Provider, account_id: &str) {
        if account_id.is_empty() {
            return;
        }
        *self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .routed_since_refresh
            .entry(score_key(provider, account_id))
            .or_default() += 1;
    }

    #[must_use]
    pub fn live_debits(&self) -> HashMap<String, usize> {
        self.0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .routed_since_refresh
            .clone()
    }

    pub fn touch(&self) {
        self.0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .updated_at = SystemTime::now();
    }

    pub fn set_updated_at(&self, updated_at: SystemTime) {
        self.0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .updated_at = updated_at;
    }

    fn prune_expired(&self, now: SystemTime) {
        let mut state = self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state.exhausted_until.retain(|_, until| *until > now);
        state.incompatible_until.retain(|_, until| *until > now);
    }
}

fn update_scheduler(state: &mut SchedulerState, scheduler: Scheduler) {
    let base = state.scheduler.clone();
    state.scheduler = scheduler;
    retain_exhaustion_expiries(state);
    let marks = expiry_marks(state);
    state.scheduler = strip_carried_overlays(&state.scheduler, &base, &marks);
}

fn retain_exhaustion_expiries(state: &mut SchedulerState) {
    let now = SystemTime::now();
    state.exhausted_until.retain(|key, until| {
        let Some((score_key, _, pool)) = exhaustion_key_parts(key) else {
            return false;
        };
        let Some(mut score) = state.scheduler.scores.get(&score_key).cloned() else {
            return false;
        };
        if !pool.is_empty() {
            let Some(model_score) = score.model_scores.get(&pool).cloned() else {
                return true;
            };
            score = model_score;
        }
        if !score.exhausted() {
            return false;
        }
        if score.fresh {
            let reset = Duration::from_secs(score.short_reset_after_seconds.max(0) as u64);
            let extension = DEFAULT_EXHAUSTED_TTL
                .max(reset)
                .min(Duration::from_secs(8 * 24 * 60 * 60));
            *until = (*until).max(now + extension);
        }
        true
    });
}

fn expiry_marks(state: &SchedulerState) -> HashMap<String, SystemTime> {
    let mut marks = state.exhausted_until.clone();
    for (key, until) in &state.incompatible_until {
        marks
            .entry(key.clone())
            .and_modify(|existing| *existing = (*existing).max(*until))
            .or_insert(*until);
    }
    marks
}

fn pool_scoped_key(provider: Provider, account_id: &str, pool: &str) -> String {
    format!("{}\0{}", score_key(provider, account_id), model_key(pool))
}

fn exhaustion_key_parts(key: &str) -> Option<(String, Provider, String)> {
    let mut parts = key.splitn(3, '\0');
    let provider: Provider = parts.next()?.parse().ok()?;
    let account_id = parts.next()?;
    let pool = parts.next()?.to_owned();
    Some((score_key(provider, account_id), provider, pool))
}

fn apply_exhaustion_marks(
    base: &Scheduler,
    marks: &HashMap<String, SystemTime>,
    now: SystemTime,
) -> Scheduler {
    if marks.is_empty() {
        return base.clone();
    }
    let mut next = base.clone();
    let introduced: HashSet<String> = marks
        .iter()
        .filter(|(_, until)| **until > now)
        .filter_map(|(key, _)| exhaustion_key_parts(key).map(|(_, _, pool)| pool))
        .filter(|pool| !pool.is_empty() && !base.has_model_score(pool))
        .collect();
    for pool in introduced {
        for score in next.scores.values_mut() {
            if !score.model_scores.contains_key(&pool) {
                let mut pool_score = score.clone();
                pool_score.model_scores.clear();
                score.model_scores.insert(pool.clone(), pool_score);
            }
        }
    }
    let mut account_wide = HashSet::new();
    for (key, _) in marks.iter().filter(|(_, until)| **until > now) {
        let Some((score_key, provider, pool)) = exhaustion_key_parts(key) else {
            continue;
        };
        if !pool.is_empty() {
            continue;
        }
        let account_id = score_key.split_once('\0').map_or("", |(_, id)| id);
        let score = next
            .scores
            .entry(score_key.clone())
            .or_insert_with(|| Score {
                account_id: account_id.into(),
                provider,
                ..Score::default()
            });
        score.provider = provider;
        score.headroom = 0.0;
        score.short_headroom = 0.0;
        score.model_scores.clear();
        account_wide.insert(score_key);
    }
    for (key, _) in marks.iter().filter(|(_, until)| **until > now) {
        let Some((score_key, provider, pool)) = exhaustion_key_parts(key) else {
            continue;
        };
        if pool.is_empty() || account_wide.contains(&score_key) {
            continue;
        }
        let account_id = score_key
            .split_once('\0')
            .map_or("", |(_, id)| id)
            .to_owned();
        let score = next.scores.entry(score_key).or_insert_with(|| Score {
            account_id,
            provider,
            headroom: 1.0,
            short_headroom: 1.0,
            ..Score::default()
        });
        score.provider = provider;
        score.model_scores.insert(
            pool,
            Score {
                account_id: score.account_id.clone(),
                provider,
                ..Score::default()
            },
        );
    }
    next
}

fn strip_carried_overlays(
    current: &Scheduler,
    base: &Scheduler,
    marks: &HashMap<String, SystemTime>,
) -> Scheduler {
    if marks.is_empty() {
        return current.clone();
    }
    let mut next = current.clone();
    for key in marks.keys() {
        let Some((score_key, _, pool)) = exhaustion_key_parts(key) else {
            continue;
        };
        if !pool.is_empty() && !base.has_model_score(&pool) {
            for score in next.scores.values_mut() {
                if score
                    .model_scores
                    .get(&pool)
                    .is_some_and(|model| !model.fresh)
                {
                    score.model_scores.remove(&pool);
                }
            }
        }
        let Some(score) = next.scores.get(&score_key).cloned() else {
            continue;
        };
        if pool.is_empty() {
            if score.exhausted() && !score.fresh {
                if let Some(base_score) = base.scores.get(&score_key) {
                    next.scores.insert(score_key, base_score.clone());
                } else {
                    next.scores.remove(&score_key);
                }
            }
            continue;
        }
        if !score
            .model_scores
            .get(&pool)
            .is_some_and(|model| model.exhausted() && !model.fresh)
        {
            continue;
        }
        let mut updated = score;
        if let Some(base_model) = base
            .scores
            .get(&score_key)
            .and_then(|base_score| base_score.model_scores.get(&pool))
        {
            updated.model_scores.insert(pool, base_model.clone());
        } else {
            updated.model_scores.remove(&pool);
        }
        next.scores.insert(score_key, updated);
    }
    next
}

#[cfg(test)]
mod tests {
    use super::*;

    fn account(id: &str, provider: Provider, auth_mode: AuthMode) -> Account {
        Account {
            id: id.into(),
            provider,
            auth_mode,
            token: "token".into(),
            ..Account::default()
        }
    }

    #[test]
    fn oauth_headroom_beats_api_key_then_falls_back() {
        let scheduler = Scheduler::new([Score {
            account_id: "oauth".into(),
            provider: Provider::Codex,
            headroom: 0.5,
            short_headroom: 0.5,
            ..Score::default()
        }]);
        let candidates = [
            account("key", Provider::Codex, AuthMode::ApiKey),
            account("oauth", Provider::Codex, AuthMode::Oauth),
        ];
        assert_eq!(scheduler.pick(&candidates).unwrap().id, "oauth");
        let cooked = Scheduler::new([Score {
            account_id: "oauth".into(),
            provider: Provider::Codex,
            ..Score::default()
        }]);
        assert_eq!(cooked.pick(&candidates).unwrap().id, "key");
    }

    #[test]
    fn model_pools_are_strict_and_provider_scoped() {
        assert_eq!(model_key("GPT-5.3-Codex-Spark"), "gpt53codexspark");
        let score = score_from_limit_windows(
            "same",
            0,
            &[
                LimitWindow {
                    used_percent: 10.0,
                    limit_window_seconds: 300,
                    ..LimitWindow::default()
                },
                LimitWindow {
                    used_percent: 100.0,
                    limit_window_seconds: 300,
                    feature: "GPT-5.3-Codex-Spark".into(),
                    ..LimitWindow::default()
                },
            ],
        );
        let mut claude = score.clone();
        claude.provider = Provider::Claude;
        let scheduler = Scheduler::new([score, claude]);
        assert!(!scheduler.exhausted(Provider::Codex, "same"));
        assert!(
            scheduler
                .for_model("gpt-5.3-codex-spark")
                .exhausted(Provider::Codex, "same")
        );
    }

    #[test]
    fn expiry_overlay_recovers_and_live_debit_never_marks_exhausted() {
        let scheduler = Scheduler::new([Score {
            account_id: "a".into(),
            provider: Provider::Claude,
            headroom: 1.0,
            short_headroom: 1.0,
            ..Score::default()
        }]);
        let reference = SchedulerRef::new(scheduler);
        reference.mark_exhausted_until(
            Provider::Claude,
            "a",
            "",
            SystemTime::now() - Duration::from_secs(1),
        );
        assert!(!reference.get().exhausted(Provider::Claude, "a"));
        for _ in 0..1_000 {
            reference.note_routed(Provider::Claude, "a");
        }
        assert!(
            !reference
                .get()
                .with_live_debits(reference.live_debits())
                .exhausted(Provider::Claude, "a")
        );
    }
}
