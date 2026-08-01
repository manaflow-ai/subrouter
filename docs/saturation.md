# Saturation Strategy

Codex subscription routing has two practical constraints: keep a conversation sticky to preserve caching, and choose a good account only when a new conversation starts.

## Score

For each account, convert all known windows into remaining headroom:

```text
remaining_5h = 1 - used_5h_percent
remaining_7d = 1 - used_7d_percent
headroom = min(remaining_5h, remaining_7d)
expiry_pressure = headroom / seconds_until_5h_reset
```

The `min` is the bottleneck. If an account has 95% free in the 5h window but only 5% free in the 7d window, it should not receive new sessions unless every other account is worse.

The expiry pressure is the tiebreaker for otherwise healthy accounts. It spends capacity that would disappear soonest, while avoiding accounts whose remaining 5h capacity is already tight for an unknown new session.

## Selection

For a new conversation:

1. Keep existing sessions on their assigned account.
2. Prefer usable subscription OAuth before API-key fallback.
3. Protect OAuth accounts below 40% bottleneck or 5h headroom.
4. Pick the healthy account with highest expiry pressure.
5. Fall back to highest bottleneck headroom.
6. Break ties by fewer active sessions.

## Why this saturates better

Round-robin wastes capacity when one account is near a 5h or 7d cap. Pure bottleneck-headroom routing also leaves value unused when multiple accounts are healthy but one 5h bucket resets much sooner. Expiry-aware bottleneck routing spends soon-expiring healthy quota first, then falls back to the fullest safe account.

Scheduler simulations in `src/selectacct.rs` cover competing short and long quota windows, expiry pressure, live debits, and exhausted-account recovery.

The scheduler always scores the limiting quota window, so unused capacity in another window cannot hide exhaustion.

## Current implementation

Subrouter fetches Codex OAuth account usage on startup by default. Use `--fetch-usage=false` to skip live usage checks and fall back to static account ordering.

Accounts whose usage fetch fails are scored at zero headroom so stale tokens do not win new sessions.
