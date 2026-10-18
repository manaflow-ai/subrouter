# Offline token debit evidence

Run from a checkout with a local, private export of the token ledger:

```sh
go run ./cmd/subrouter-debit-calibration -ledger token-usage.jsonl
```

The command also accepts the `/_subrouter/token-usage` endpoint's JSON response
and reads stdin with `-ledger -`. It opens only the supplied file, makes no
network requests, and writes JSON to stdout. No credentials or setup are needed
to analyze a file already available locally. This does not fetch private server
data or expose a new endpoint.

The report groups provider/model totals and account-hour means. It gives
request counts, the fraction missing token usage, and separate input, cached
input, cache-write input, and output means per request with usage. Cache reads
and writes are already subsets of normalized input. They are not added a
second time. Account and client labels are used only to merge buckets and are
not emitted. Model labels remain visible. Keep input exports and reports
private; aggregate counts can still describe sensitive activity.

Use a single ledger snapshot or exports with disjoint time ranges. The ledger
contains aggregate counters, not event IDs, so overlapping snapshots cannot be
deduplicated. Min/max values describe account-hour averages, not individual
request sizes. Missing usage produces null means when no request in a group has
token usage. Partial hours remain partial hours.

## What this can establish

Different model/account-hour token mixes can show whether request count alone
is a coarse approximation. This is descriptive evidence, not a measured token
cost in provider quota. The tool deliberately returns
`quota_calibration_supported: false` and changes no scheduler settings.

## What is missing for a token-weighted routing debit

The token ledger is hourly and finalized after responses. Current usage status
and startup score caches are last-known observations, not a historical series
of quota changes aligned with those rows. A calibration needs:

- Fresh account-wide provider-window observations at both ends of each interval,
  with observation timestamps and matching reset identity. Stale reads,
  resets, rolling-window replenishment, and rounded percentages must be handled.
- Plan and model context, plus separation of subscription windows and paid
  extra usage. Input, output, cached reads, and cache writes must remain separate
  rather than assuming their provider quota weights.
- Attribution or exclusion of usage outside this server and requests without
  token usage. Otherwise a provider-window delta includes unobserved demand.
- Request completion and admission timing for a concurrent replay. Hourly totals
  cannot reconstruct the 50-request burst or establish what was knowable at
  placement time. A debit based on completed tokens also needs a measured
  fallback for new or still-running sessions.

No quota conversion or new default should be chosen from these hourly averages
alone. Additional observations can stay local and contain counters and window
metadata only; request bodies and credentials are unnecessary.
