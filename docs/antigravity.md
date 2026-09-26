# Antigravity (AGY) routed profiles

Subrouter supports pooled AGY OAuth through AGY's supported `CLOUD_CODE_URL`
override. `sr agy` starts the normal AGY CLI but sends its Cloud Code requests
through a short-lived local relay; the server selects and authenticates the
isolated account. Plain `agy` remains direct and is not modified.

## Add and launch profiles

1. Sign plain `agy` into one Google account.
2. Run `sr agy add <label>` with the intended server selected.
3. Sign plain `agy` into the next account and repeat with a different label.
4. Run `sr status` and confirm each verified email appears as its own profile.
5. Run `sr agy` for deterministic pooled selection, or
   `sr agy --account <label-or-email>` to pin a process.

`sr agy` without `--account` pools the imported server accounts, retaining
session affinity and failing over bounded authentication/rate-limit errors.
Transient 429 responses are retried/fail over for the request but do not by
themselves mark an account exhausted; only an authoritative account-wide
failure should remove it from later selection.
`sr agy --account <label-or-email>` hard-pins one account. The local AGY login
is used only to pass the CLI's startup check; it is not the routed identity.

Cloud Code's `retrieveUserQuotaSummary` is a weekly/model-family summary, not a
guarantee that every generation allocation is available. A generation can
still return `429 RESOURCE_EXHAUSTED` for a model or session while `sr status`
shows remaining quota. Subrouter records the redacted upstream reason and tries
the other pooled account once within the request budget; if both accounts are
rejected, the upstream error is returned rather than hidden or retried without
bound.

## Recovery and acceptance evidence

If the host or process is hard-killed during a native launch, run:

```sh
sr server use local
sr agy recover
```

The recovery command replays the durable swap journal and restores the prior
Keychain slot. Do not delete the journal manually.

For an opt-in Darwin canary, use two already-authorized accounts and record
credential-free evidence for each launch: selected email after startup, one
minimal model request, refresh behavior, clean exit, and `sr status` afterward.
Only after both sequential launches pass may a concurrent test be attempted;
do not claim concurrent native pooling unless both processes retain their
identity through startup, request, refresh, and exit. A failed concurrent test
must leave native mode serialized and server-side pooling unchanged.

Plain `agy` remains direct and is outside Subrouter's managed profile lock.

Current acceptance note: a disposable shadow run with two verified OAuth
profiles reached the real AGY Cloud Code endpoints. Using the consumer
`daily-cloudcode-pa.googleapis.com` upstream, pinned launches for both
profiles and a bare pooled launch each returned a real 2xx Gemini generation.
The prior `cloudcode-pa.googleapis.com` default produced misleading upstream
`429 RESOURCE_EXHAUSTED` responses. Native Cloud Code generation also carries
a top-level `project` bound to the OAuth identity. The relay discovers and
caches the selected account's project when available, rewriting only that
top-level field; valid control responses without a project remain usable.
