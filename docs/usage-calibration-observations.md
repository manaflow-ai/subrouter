# Private quota and token observations

Single-tenant workers collect calibration-only observations in
`usage-calibration/observations.jsonl` beside the token ledger. There is no new
endpoint, network exporter, or routing coefficient. Multi-tenant workers do not
enable this collector.

Each accepted, new account-wide provider observation records:

- Provider, plan label, opaque account hash, account-set generation, and a random
  worker epoch. The generation changes with account additions/removals or
  credential rotations; comparing different generations is unsafe.
- Provider observation time separately from the time counters were sampled.
- Subscription-window name, used percent, duration, and absolute reset time.
  Model-specific and paid-extra windows are excluded. Unknown reset times stay
  unknown rather than becoming an invented reset identity.
- Cumulative completed-request counts and normalized input, cached-read,
  cache-write, and output tokens by model, including requests missing usage.
  Counters start at worker startup and survive hourly ledger rollover. They do
  not reload old ledger totals or pretend to include another worker's traffic.
- Sequence number, cumulative dropped-observation count, and explicit
  uncertainty labels.

The collector projects these fields rather than serializing account status.
It does not persist email, account/client/device names, sources, credentials,
request bodies, response bodies, or session identifiers. Account hashes and
aggregate usage are still private metadata, not anonymized public data.

Repeated cached observations are deduplicated by account, generation, and
provider observation time. Stale, throttled, empty, and model-only observations
do not create samples. This uses the existing usage-sweep and score-refresh
persistence seam and makes no extra provider calls.

The directory is mode 0700 and data files are mode 0600. The existing
cross-platform nonblocking file-lock implementation is used inside this
dedicated directory, so it does not contend with account imports. It protects
append and rotation across overlapping workers. At most two 32 MiB segments
are retained. This is a byte bound, not a promise of a fixed number of days.
The request path only updates bounded in-memory counters. A separate writer
drains a 64-record queue, each at most 64 KiB; busy locks and I/O failures drop
observations rather than delaying inference. Counters/observation identities
are capped at the existing accounting key limit; capacity or overflow marks
subsequent samples incomplete.

These samples are **not atomic provider/token snapshots**. Provider counters
can lag, quota can replenish, streams can be in flight, and capture can race an
account-generation change. Both capture timestamps and uncertainty labels are
retained so a future offline analysis can exclude uncertain intervals. Worker
epochs must remain separate during canary overlap. Outside-server traffic and
upstream errors with unknown usage are unverified demand. Sequence gaps,
changed drop counts, missing reset identities, counter resets, generation
changes, and incomplete counters also require exclusions.

This collection supplies evidence for a future measured calibration. It does
not make the hourly token ledger sufficient for request-order replay, establish
provider token weights, or authorize a token-based quota debit.

To inspect available private files from a checkout without credentials or
network access (omit the rotated file if it does not exist):

```sh
python3 scripts/inspect-usage-calibration.py usage-calibration/observations.jsonl.1 usage-calibration/observations.jsonl --limit 10
```

The report gives interval counts, exclusion counts, and recent descriptive
window/counter deltas with both capture times and uncertainty labels. It strips
account keys and worker identifiers from output. Worker/generation changes,
counter resets, reset-window changes, sequence/drop gaps and missing token
usage exclude intervals. Gaps are checked across the whole worker sequence,
including other accounts, so normal interleaved account observations are not
mistaken for dropped records. Do not filter to one account before inspection.

Schema 1 cannot establish complete demand coverage, so every interval remains
ineligible for quota calibration. Cross-worker/generation counters and changed
reset windows do not get fabricated comparable deltas. No weights are fitted.
Reading a service-owned file still requires local filesystem permission; this
command does not grant access to another user's private data.

Reader checks run with:

```sh
python3 -m unittest discover -s scripts -p test_usage_calibration.py
```
