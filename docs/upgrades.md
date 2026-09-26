# Subrouter upgrades

Use this runbook for local macOS daemon upgrades when Codex is already pointed at `127.0.0.1:31415`.

## Replacing the binary in place on macOS

A LaunchAgent that runs `subrouter serve` directly, rather than behind
`subrouter supervise`, is upgraded by replacing its executable. **Do not
overwrite the live executable with `cp`.** Writing through the existing inode
invalidates the binary's code-signing state, and macOS then kills every respawn
with `OS_REASON_CODESIGNING` (SIGKILL, exit 137). The daemon appears to
flap: launchd restarts it, the kernel kills it, and the log shows nothing
useful.

Restoring the previous binary to the same path does **not** recover it. The
pathname stays poisoned even when `codesign --verify --strict` passes and the
restored file is byte-identical to the original. Recovery requires deleting the
file first, so the copy lands on a fresh inode:

```bash
set -euo pipefail
cp ~/bin/subrouter.backup ~/bin/subrouter.restore
chmod 755 ~/bin/subrouter.restore
codesign --verify --strict --verbose=4 ~/bin/subrouter.restore
~/bin/subrouter.restore --help >/dev/null
launchctl bootout gui/$(id -u)/<label>
mv -f ~/bin/subrouter.restore ~/bin/subrouter
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist
```

The upgrade procedure that avoids this staged the new binary under its own name,
signs and proves it runs *before* the old one is taken out of service, and puts
it in place with an atomic rename:

```bash
set -euo pipefail
cp subrouter.new ~/bin/subrouter.new
# Preserve release signatures. A failed release verification is fatal; only an
# explicitly selected local build may replace its absent signature ad hoc.
if ! codesign --verify --strict ~/bin/subrouter.new 2>/dev/null; then
  [ "${SUBROUTER_LOCAL_BUILD:-0}" = 1 ] || {
    echo "release artifact signature verification failed" >&2
    exit 1
  }
  codesign --force --sign - ~/bin/subrouter.new
fi
codesign --verify --strict --verbose=4 ~/bin/subrouter.new
~/bin/subrouter.new --help >/dev/null            # prove it executes first
cp -p ~/bin/subrouter ~/bin/subrouter.rollback
launchctl bootout gui/$(id -u)/<label>
mv ~/bin/subrouter.new ~/bin/subrouter           # rename, never cp
if ! launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist; then
  mv -f ~/bin/subrouter.rollback ~/bin/subrouter
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist
  exit 1
fi
rm -f ~/bin/subrouter.rollback
```

On macOS versions whose launchd policy accepts ad-hoc signatures, the explicit
local-build fallback makes an unsigned build runnable without replacing a valid
release signature. A deployment using `SpawnConstraint` with a
`team-identifier` must instead use a certificate-backed signature for that team;
an ad-hoc signature has no Team ID and cannot satisfy the constraint.

### What a restart costs

`serve` drains on SIGTERM: it stops accepting, finishes in-flight requests, and
exits, bounded by `--shutdown-timeout` (default 10 minutes). launchd is the
shorter fuse. Without an explicit `ExitTimeOut` in the direct `install-daemon`
plist, the escalation timeout is system-defined and may be shorter than
`--shutdown-timeout`; a stream still running when launchd escalates is cut, and
`ThrottleInterval` delays the restart by its value. Sticky session assignments
survive, because the session store is read back from its file at startup; the
scheduler's exhaustion marks do not, since they are in-memory only.

During a supervised worker upgrade, the supervisor keeps owning the listener
and hands existing connections to their original worker, avoiding listener
interruption and connection drops. Restarting the supervisor itself still
closes the listener and uses the normal drain/restart behavior.

## Credential-origin rollback boundary

The rollback binary must understand the stored Codex
`oauthCredentialOrigin` field once `sr codex migrate-isolation` has run. A
binary from before that field existed ignores it while reading an account and
can erase it the next time it refreshes and rewrites the credential. The next
upgraded daemon then rejects that account as unisolated.

Do not keep a pre-isolation binary as the post-migration rollback artifact;
retain the last build that preserves credential origin instead. If an emergency
rollback did run an older binary, stop it and rerun
`sr codex migrate-isolation` before starting an isolation-enforcing build. That
re-enrollment may require browser approval for every account the older binary
rewrote.

## Supervised handoff

On macOS, run Subrouter behind `subrouter supervise`. The supervisor owns the public listener, starts each worker on an inherited private socket, and pins accepted connections to that worker generation. Configure `--local-data-socket` in a current-user-owned mode-0700 directory; the supervisor creates that stable socket mode 0600 and routes local credential-bearing CLI traffic through the same generation switch. Publish it with `sr daemon bind-state STATE_DIR --local-data-socket PATH` only after the socket's exact supervisor ownership, kernel identity, and store handshake succeed. The v2 binding pins that socket identity, and each new connection repeats the mutual store handshake before credentials can be sent. Preserve the exact prior binding for rollback and restore it together with the prior socket/service state. Root or system services must explicitly provide a user-readable private metadata handoff or use a named authenticated remote; never infer a per-user path. An upgrade starts and health-checks the replacement before switching new connections. Old WebSockets, SSE streams, HTTP requests, and keep-alive connections remain on the old worker. The old worker exits only after its connection count reaches zero.

The supervisor is deliberately separate from the replaceable worker binary. Routine releases update `/usr/local/bin/subrouter`; they do not replace or restart `/usr/local/libexec/subrouter-supervisor`.

### One-time LaunchDaemon migration

Preparation does not touch the running service:

```bash
sudo ./deploy/macos/migrate-launchdaemon-to-supervisor.sh
```

Inspect the generated `.plist.supervised`, then perform the one-time listener transition:

```bash
sudo ./deploy/macos/migrate-launchdaemon-to-supervisor.sh --activate
```

The one-time transition cannot preserve connections accepted by an older, unsupervised process because that process owns their file descriptors. Perform it in a maintenance window. All later worker upgrades preserve connections.

### Transactional per-user LaunchAgent migration

This migration is an optional high-assurance path for sensitive changes. Choose
one of three credential layouts:

1. **Full isolated production cutover (default):** run `enroll-isolated`
   without `--only`. The complete retiring Codex OAuth inventory is enrolled
   into a separate candidate store, allowing the full activation preflight and
   an independently usable legacy service.
2. **Selected isolated validation only:** repeat `--only ACCOUNT` to enroll
   explicit accounts for offline or canary validation. A partial candidate is
   useful for those checks, but the complete-inventory preflight still blocks
   full activation.
3. **Ordinary in-place upgrade:** keep the original state root and use
   `sr codex migrate-isolation` when required. This avoids a second inventory,
   but it does not provide an independent credential rollback guarantee.

#### Optional shadow rehearsal before activation

For a sensitive or heavily used deployment, first run the exact candidate as a
disposable shadow on an unused loopback listener. The host-neutral helper pins
the candidate by SHA-256 into a private temporary workspace, gives a preparation
callback that workspace's isolated state directory, starts `serve`, waits for
health and readiness from that exact candidate, and runs the authenticated
canary callback. It then re-proves candidate ownership and readiness, stops the
complete process group, removes its state and logs, proves the process, listener,
and workspace are absent, and prints one bounded JSON evidence record:

```bash
candidate=/absolute/path/subrouter
candidate_sha256="$(shasum -a 256 "$candidate" | awk '{print $1}')"
SUBROUTER_ADMIN_TOKEN_FILE=/private/shadow-admin-token \
SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/private/shadow-import-token \
deploy/run-shadow-rehearsal.py \
  --candidate "$candidate" \
  --candidate-sha256 "$candidate_sha256" \
  --addr 127.0.0.1:UNUSED_PORT \
  --prepare-callback /private/prepare-shadow-state \
  --canary-callback /private/run-shadow-canary \
  --serve-args-json /private/shadow-serve-args.json
```

Both callbacks are invoked directly without a shell. They receive
`SUBROUTER_SHADOW_WORKSPACE`, `SUBROUTER_SHADOW_STATE_DIR`,
`SUBROUTER_SHADOW_CANDIDATE_PATH`, `SUBROUTER_SHADOW_BASE_URL`, and the exact
candidate SHA in `SUBROUTER_SHADOW_CANDIDATE_SHA256`. The optional serve-args
file is a JSON array of argument strings after `serve`; it cannot replace
`serve` or `--addr`. Persistent state, log, and configuration destinations
(`--sessions`, `--transcripts`, `--cloud-config`, `--transcript-gcs-uri`, and
`--transcript-azure-url`) are rejected so neither local nor remote rehearsal
artifacts can escape the disposable workspace. Raw credential flags (`--admin-token`,
`--account-import-token`, Stack keys and tenant secrets, and the Bedrock gateway
token) are also rejected because command arguments are externally observable.
Set the corresponding `SUBROUTER_*_FILE` variable in the helper environment
where one exists; both callbacks and the candidate inherit that environment.
The Bedrock gateway currently has only `SUBROUTER_BEDROCK_GATEWAY_TOKEN`, which
is still safer than a process argument. Keep every referenced credential file
private and outside the disposable workspace so teardown does not remove it.
The helper always injects a private `--sessions` path, pins
`SUBROUTER_CLOUD_CONFIG` inside the disposable state directory, and removes
inherited transcript-sync settings. It also rejects `--bedrock-autobump`, so a
rehearsal cannot request a real external quota change. The private account root
is sealed before either callback runs, preventing compatibility fallback to a
live legacy account directory under the invoking user's home. Shadow serve also
disables the fixed-service Antigravity Keychain source; validate that live
credential separately rather than exposing it to a disposable rehearsal.
Callbacks run with a one-use descendant marker. Teardown drains both their
original process group and any marked child that detached into another group;
leaving a descendant makes the rehearsal fail.
Callbacks are trusted deployment programs, not sandboxed input. A callback can
already use its authorized credentials and can deliberately evade cooperative
descendant marking by sanitizing its environment; review callback bytes and
ownership with the same care as the candidate. The marker protects against
ordinary and accidental daemonization, not hostile callback code.

The helper creates its own one-run health key in the private workspace and gives
only the candidate its file path. Each ownership probe sends a fresh random
challenge and requires the candidate's HMAC proof, once before and once after
the canary. A different process that wins the listener race therefore cannot
make the rehearsal pass by returning generic healthy JSON. The key is never an
argument, is removed as soon as the candidate has loaded it, and is not given to
either callback. Ordinary health requests remain unchanged; the proof field is
present only when this private key is configured and a valid explicit challenge
header is supplied.

Success requires `"ok":true` and every field under `"teardown"` to be true. A
callback failure, SIGINT, SIGTERM, or—on platforms that provide them—SIGHUP or
SIGQUIT still runs teardown and returns nonzero evidence. SIGKILL cannot run any
userspace cleanup handler, so after an unclean host interruption verify the
configured listener port is free before retrying.

Shadow rehearsal and live rollback are complementary, not substitutes. A
passing shadow is a recommended high-assurance activation gate for sensitive
deployments, not a universal requirement. When used, the preserved legacy
service and rollback bundle still remain armed until the live candidate passes
health/readiness, authenticated routed traffic, and an existing idle session's
next turn. Put deployment-specific peers, accounts, sessions, and transports
in private canary configs rather than source so the procedure works with or
without a tailnet and never hardcodes one operator's fleet.

Use a separate candidate state root when rollback must preserve an independently
usable legacy service. Re-enroll every served OAuth account into that root so
the candidate and legacy service never share a rotating refresh-token chain:

```bash
candidate_state="$HOME/.subrouter-candidate"
retiring_state="$HOME/.subrouter"
candidate_bin="$(command -v subrouter)" # or an absolute reviewed candidate path
SUBROUTER_STATE_DIR="$candidate_state" \
  SUBROUTER_BIN="$candidate_bin" \
  "$candidate_bin" codex enroll-isolated \
  --retiring-state-dir "$retiring_state" \
  --device-auth
```

Omitting `--only` above is the production default and enrolls the complete
inventory. To prepare only named accounts for offline or canary validation,
repeat the flag, for example `--only first@example.com --only
second@example.com`. The command reports a partial candidate and leaves full
activation blocked until every retiring Codex OAuth identity is enrolled.

The mandatory `codex isolation-check` preflight enforces this separation only
for the Codex store. Add every other OAuth subscription profile through its
normal account-specific login command with the same candidate state root, and
make its identity plus a real routed request an explicit deployment canary.
Claude, Kimi, and future OAuth providers do not inherit Codex's refresh-chain
comparison automatically. Static API keys may be added to both stores without
reauthorization because using them does not rotate or invalidate the rollback
copy. Never copy OAuth refresh tokens to avoid the login step: either finish
and independently verify each provider's isolated enrollment or choose the
ordinary in-place upgrade. The generic transaction fails closed on the Codex
comparison and every declared callback; it cannot prove an undeclared
provider-specific isolation property.

Recommend this mode when a short maintenance window is acceptable but a failed
cutover must restore the complete previous provider pool. Its cost is one fresh
approval per OAuth account plus deployment-specific functional-canary setup.
Its benefit is that health, real routed Codex and Claude traffic, failover, and
an existing session can all be proven while automatic rollback remains armed.

The per-user migration does not accept health alone as cutover proof. Preparation
is non-disruptive, while activation requires a bounded preflight and an explicit
functional canary command:

```bash
SUBROUTER_STATE_DIR="$candidate_state" SUBROUTER_BIN="$candidate_bin" \
  deploy/macos/migrate-launchagent-to-supervisor.sh
SUBROUTER_STATE_DIR="$candidate_state" SUBROUTER_BIN="$candidate_bin" \
  deploy/macos/migrate-launchagent-to-supervisor.sh --activate \
  --canary-callback /path/to/real-routed-canary
```

If the existing LaunchAgent invokes only a service wrapper and therefore does
not expose the worker's `serve --addr ...` arguments in its plist, supply the
public listener and worker arguments explicitly on both preparation and
activation:

```bash
SUBROUTER_STATE_DIR="$candidate_state" SUBROUTER_BIN="$candidate_bin" \
  deploy/macos/migrate-launchagent-to-supervisor.sh \
  --public-addr 127.0.0.1:8080 \
  --worker-serve-args-json /path/to/worker-serve-args.json \
  --candidate-env-json /path/to/candidate-environment.json
SUBROUTER_STATE_DIR="$candidate_state" SUBROUTER_BIN="$candidate_bin" \
  deploy/macos/migrate-launchagent-to-supervisor.sh --activate \
  --canary-callback /path/to/real-routed-canary \
  --public-addr 127.0.0.1:8080 \
  --worker-serve-args-json /path/to/worker-serve-args.json \
  --candidate-env-json /path/to/candidate-environment.json
```

`SUBROUTER_PUBLIC_ADDR`, `SUBROUTER_WORKER_SERVE_ARGS_JSON`, and
`SUBROUTER_CANDIDATE_ENV_JSON` are equivalent environment inputs. Public address
and worker-argument JSON must be supplied together. The worker file is a JSON
array of argument strings after the `serve` subcommand; it must not include the
binary, `serve`, or any `--addr` form. The optional environment file is a JSON
object restricted to file references: every key must match
`SUBROUTER_*_FILE`, and every value must be an absolute path to an existing
regular non-symlink file owned by the current uid with no group or other
permissions. Referenced files are opened with `O_NOFOLLOW` for validation.
Raw secret values, `SUBROUTER_ADMIN_TOKEN`, relative or missing paths, symlinks,
and group/world-accessible files are rejected. The JSON inputs themselves must
also be regular non-symlink files. They are decoded as JSON and never evaluated
by a shell. Validated file-reference entries are merged only into the prepared
supervisor plist; the retained legacy plist remains byte-for-byte unchanged.

The default preflight directly executes the candidate as `subrouter codex
isolation-check --json --retiring-state-dir PATH`; it is read-only and compares
the complete candidate and retiring account inventories. It fails unless the
candidate uses its exact isolated store, every served Codex OAuth credential
has trusted provenance, and no refresh-token chain is shared within the
candidate store or with the retiring store. The retiring root is read from the
preserved plist's explicit `SUBROUTER_STATE_DIR`; use `--retiring-state-dir
PATH` only when migrating a plist that predates that declaration. Missing,
ambiguous, or equal roots fail before live mutation. `--preflight-callback
PATH` adds a deployment-specific executable after the mandatory isolation
comparison; it cannot replace or bypass the credential gate.

`SUBROUTER_STATE_DIR` selects the candidate state root and is written into the
prepared LaunchAgent explicitly; launchd does not inherit the activating
shell's environment. During migration from a binary that predates credential
provenance, point the candidate at a separately migrated state root. The
retained rollback plist must keep using the untouched legacy state root so the
old binary cannot erase provenance from candidate credentials.

Activation also snapshots the default CLI serving-store binding at
`~/.subrouter/codex/.local-serving-store.json` as either exact bytes, SHA-256,
and mode or exact absence. After structural acceptance, it invokes the reviewed
candidate binary directly as `daemon bind-state` with `SUBROUTER_STATE_DIR`
removed and an expected-prior compare-and-swap condition. The comparison and
publication occur while holding the same private
`~/.subrouter/codex/.local-serving-store.lock` used by ordinary bind and unbind
commands. A concurrent operator change therefore aborts activation without
being overwritten; the candidate and transaction journal remain available for
an explicit recovery decision.

The canary callback must be an executable file that exercises ordinary routed
traffic and returns nonzero unless the expected response is observed. Callback
paths are executed directly with no shell evaluation or command-string parsing.
Do not put credentials in filenames or arguments; have the callback read any
required file-backed credential through its normal consumer.
`SUBROUTER_PREFLIGHT_TIMEOUT` and `SUBROUTER_CANARY_TIMEOUT` bound the callbacks
(120 and 300 seconds by default); timeout terminates and waits for the callback
process group.

The callback runs with `SUBROUTER_STATE_DIR` removed. It therefore exercises
the same published serving-store selection that a normal shell-launched relay
will use, rather than succeeding through the activation shell's explicit
candidate-state override. The exact candidate binding is checked again after
the callback before rollback is disarmed.

The migration itself checks local health and readiness. The functional canary
owns every deployment-specific acceptance leg: remote health/readiness probes,
sticky or existing-session continuity, and a real routed provider response.
Keep those checks in the callback so the generic migration contains no machine
names, tailnet addresses, account identifiers, or credentials.

`deploy/macos/run-functional-canary.py` is the reusable fail-closed callback
runner for deployments that need all of those legs. It reads the absolute
private manifest path from `SUBROUTER_CANARY_MANIFEST_FILE` (or `--manifest`
when validating it directly). The manifest and every leg config must be
current-user-owned regular non-symlink files with no group or other access.
Each leg executable must be a current-user-owned, non-privileged regular file
that is not group/other-writable; normal mode `0755` binaries are accepted.
Executables are invoked directly, never through a shell, and receive their
private config path only through `SUBROUTER_CANARY_LEG_CONFIG_FILE`. The
manifest records the reviewed source Git OID explicitly as unverified context,
and pins the candidate-worker path and SHA-256,
evidence path, total timeout, and each leg's executable/config paths, hashes,
and timeout. The migration passes its actual `WORKER_BIN` path and captured
SHA-256 to the runner; the runner requires an exact manifest match before it
acquires leases or starts any leg. The runner rechecks file identities
immediately before execution.
Use the same reviewed `subrouter-cutover-canary` binary for all six legs; its
configs use schema `subrouter.cutover-canary-config/v1` and keep host-, account-,
and session-specific values outside the repository.

Build the deployment helper from the same reviewed source checkout as the
candidate, sign it on macOS, and record its exact hash in every leg entry:

```bash
install -d -m 0700 /private/deployment/path
umask 077
go build -trimpath -o /private/deployment/path/subrouter-cutover-canary \
  ./cmd/subrouter-cutover-canary
chmod 0755 /private/deployment/path/subrouter-cutover-canary
codesign -s - -f /private/deployment/path/subrouter-cutover-canary
shasum -a 256 /private/deployment/path/subrouter-cutover-canary
```

The helper is a one-time deployment artifact, not a normal end-user command or
release binary. Stage it, its private configs, and the Python runner together;
do not assume installing `subrouter` also installs the helper.

Create the mode-`0600` leg configs with the following exact shapes. Every
`http` object contains `base_url`, optional `admin_token_file`,
`timeout_seconds`, and `max_response_bytes`. Values below are placeholders, not
literal filenames or account/session IDs to copy:

```json
{"schema":"subrouter.cutover-canary-config/v1","proof_file":"/private/peer-proof.json","peers":[{"name":"peer-a","ssh_host":"user@direct-host-or-address","ssh_identity_file":"/private/owner-only-ssh-key","remote_executable":"/private/subrouter-cutover-canary","remote_config_file":"/private/peer-a.json","expected_identity_kind":"darwin-cdhash-sha256","expected_executable_identity":"CDHASH","timeout_seconds":30}]}
{"schema":"subrouter.cutover-canary-config/v1","http":{"base_url":"http://127.0.0.1:PORT","admin_token_file":"/private/admin-token","timeout_seconds":30,"max_response_bytes":1048576},"proof_file":"/private/auth-proof.json","state_file":"/private/auth-state.json","cleanup_journal":"/private/auth-journal.json","model":"MODEL"}
{"schema":"subrouter.cutover-canary-config/v1","http":{"base_url":"http://127.0.0.1:PORT","admin_token_file":"/private/admin-token","timeout_seconds":30,"max_response_bytes":1048576},"proof_file":"/private/sticky-proof.json","state_file":"/private/auth-state.json","cleanup_journal":"/private/auth-journal.json","model":"MODEL"}
{"schema":"subrouter.cutover-canary-config/v1","http":{"base_url":"http://127.0.0.1:PORT","admin_token_file":"/private/admin-token","timeout_seconds":30,"max_response_bytes":1048576},"proof_file":"/private/failover-proof.json","cleanup_journal":"/private/failover-journal.json","model":"MODEL","unavailable_account_id":"ACCOUNT_ALREADY_AT_100_PERCENT"}
{"schema":"subrouter.cutover-canary-config/v1","http":{"base_url":"http://127.0.0.1:PORT","admin_token_file":"/private/admin-token","timeout_seconds":30,"max_response_bytes":1048576},"proof_file":"/private/existing-proof.json","selection_file":"/private/existing-selection.json","challenge_file":"/private/existing-challenge.json","witness_file":"/private/existing-witness.json","wait_seconds":90,"candidate_log_files":["/private/candidate.log"],"max_log_append_bytes":1048576}
```

On macOS, obtain `CDHASH` from the staged, signed helper with
`codesign -dvvv /private/subrouter-cutover-canary 2>&1` and copy its lowercase
`CDHash=` value. The peer reports the kernel-bound code-directory hash of its
running process image via `csops`; it never reopens the executable pathname, so
replacing that pathname after `exec` cannot make an older process attest newer
bytes. Non-macOS validation uses the explicit `go-build-info-sha256` identity
kind, which hashes build metadata embedded in the running image.

All six manifest legs must name the same reviewed helper path and SHA-256; the
runner rejects a mix of otherwise-valid executables. The peer's referenced probe config is
`{"schema":"subrouter.cutover-canary-config/v1","http":{...}}`. The peer leg
does not execute arbitrary local argv: it invokes `/usr/bin/ssh` with fixed
non-forwarding, non-interactive, no-backgrounding options, ignores SSH config,
and runs only the declared absolute remote helper as `peer-probe --config
REMOTE_CONFIG`. Consequently `ssh_host` must be a directly resolvable host,
address, or `user@host`, not an SSH-config-only alias. Acceptance also requires
the remote helper to report the configured kernel-bound CDHash captured from
its running process image before any peer network probe. The optional
`ssh_identity_file` is an absolute, owner-only key path on the
activating machine. When present, the helper passes it with `-i` and forces
`IdentitiesOnly=yes` plus `IdentityAgent=none`; the key path is validated as a
private regular non-symlink and may not alias any writable canary artifact.
This preserves the ignored-SSH-config boundary for hosts that do not accept a
default key or agent identity. The existing selection file is
`{"schema":"subrouter.cutover-canary-selection/v1","agent_type":"codex","session_id":"IDLE_EXISTING_SESSION"}`.
Use separate proof files for the authenticated and sticky legs, but the same
state and cleanup-journal paths so their sanitized handoff is continuous.

The private mode-`0600` manifest has this exact top-level shape; include all
six leg objects in the order below and pin every path by SHA-256. The Claude
leg config is `{"schema":"subrouter.cutover-canary-config/v1","http":{...},"proof_file":"/private/claude-proof.json","cleanup_journal":"/private/claude-journal.json","model":"CLAUDE_MODEL"}`:

```json
{"schema":"subrouter.launchagent-functional-canary/v1","source_git_oid_unverified":"FULL_40_CHARACTER_OID","candidate_worker":{"path":"/private/subrouter","sha256":"SHA256"},"evidence_file":"/private/evidence.json","total_timeout_seconds":270,"legs":[{"name":"peer-health-readiness","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/peer-leg.json","config_sha256":"SHA256","timeout_seconds":30},{"name":"authenticated-routed-codex","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/auth-leg.json","config_sha256":"SHA256","timeout_seconds":45},{"name":"sticky-reuse","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/sticky-leg.json","config_sha256":"SHA256","timeout_seconds":10},{"name":"safe-failover-reuse","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/failover-leg.json","config_sha256":"SHA256","timeout_seconds":60},{"name":"authenticated-routed-claude","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/claude-leg.json","config_sha256":"SHA256","timeout_seconds":30},{"name":"existing-session-next-turn","executable":"/private/subrouter-cutover-canary","executable_sha256":"SHA256","config_file":"/private/existing-leg.json","config_sha256":"SHA256","timeout_seconds":95}]}
```

`source_git_oid_unverified` is an operator-supplied review reference, not a
claim that the candidate bytes authenticate that source revision. Keep this
name until the release binary carries independently readable, enforced source
metadata; do not relabel it as verified provenance based only on the manifest.

Validate those exact files before the maintenance window, then pass the runner
as the migration callback:

```bash
candidate_state="$HOME/.subrouter-candidate"
candidate_worker=/private/subrouter
candidate_worker_sha256="$(shasum -a 256 "$candidate_worker" | awk '{print $1}')"
SUBROUTER_CANARY_TRANSACTION_WORKER_PATH="$candidate_worker" \
  SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256="$candidate_worker_sha256" \
  SUBROUTER_CANARY_MANIFEST_FILE=/private/manifest.json \
  deploy/macos/run-functional-canary.py --validate-only
SUBROUTER_STATE_DIR="$candidate_state" SUBROUTER_BIN="$candidate_worker" \
  SUBROUTER_CANARY_MANIFEST_FILE=/private/manifest.json \
  deploy/macos/migrate-launchagent-to-supervisor.sh --activate \
  --canary-callback /absolute/path/run-functional-canary.py
```

For gate 6, wait for `existing-challenge.json`, send its exact `prompt` through
the already-idle selected Codex session, and pass only that session's exact
one-line response to the witness command after the challenge's `not_before`:

```bash
printf '%s\n' 'EXACT_OBSERVED_MARKER' | \
  /private/subrouter-cutover-canary witness \
  --challenge /private/existing-challenge.json \
  --witness /private/existing-witness.json
```

The witness command records evidence; it does not send the Codex turn itself.
An early, extra, or mismatched response fails closed.

The manifest schema `subrouter.launchagent-functional-canary/v1` requires these
legs exactly once and in this order:

1. `peer-health-readiness`
2. `authenticated-routed-codex`
3. `sticky-reuse`
4. `safe-failover-reuse`
5. `authenticated-routed-claude`
6. `existing-session-next-turn`

Every leg must emit only this bounded JSON record, with its own exact name:

```json
{"schema":"subrouter.launchagent-functional-canary-leg/v1","leg":"peer-health-readiness","ok":true}
```

The runner rejects unknown fields, wrong order, duplicate or missing legs,
unsafe paths, timeout budgets above 270 seconds, malformed or oversized output,
nonzero exits, and mismatched evidence. It terminates the complete child process
group on timeout or signal. Each leg inherits the callback's isolated process
group so the migration's outer timeout cannot leave a nested leg running after
rollback. The migration shell owns the callback process-group identity and
synchronously drains it after any watchdog outcome, including watchdog crash
or SIGKILL, before rollback can begin. Process inspection is retried with
rollback withheld; if the recorded group identity is invalid and termination
cannot be established, migration fail-stops with the candidate and transaction
journal retained instead of risking overlapping rollback traffic. The durable
record binds the numeric process group to its leader's kernel start identity,
which is revalidated before every numeric group sample and signal. Once the
leader is absent or its identity differs, group signaling stays permanently
disabled so delayed reentry cannot signal an unrelated group after PID/PGID
reuse; cleanup continues only for token-bound descendants whose individual
kernel start identities still match. A stable signal-ignoring group anchor
remains until same-group descendants are gone, while an inherited random
callback token lets the drain find and identity-check descendants that create a
new session. It additionally tags the reviewed helper's inherited child environment
and detects a rapid reparent/session escape after the helper exits. This is a
defense for the audited helper and fixed SSH tree, not a claim that an
unprivileged Python process can contain arbitrary hostile macOS code. The
runner executes private pinned copies whose bytes are rehashed
while copying, closing the hash-to-exec/config-open replacement window. It
never repeats child stdout or stderr, and atomically writes a mode-`0600`
evidence record containing source/run identity, timestamps, hashes, leg names,
durations, status, and a redacted failure reason. Run
`--validate-only` before the maintenance window. Put deployment
endpoints, account/session selectors, and credential file references only in
the private leg configs; never put raw credentials in the manifest, arguments,
filenames, output, or source.

The authenticated and sticky gates use two tiny provider turns total. Each is
bounded by the configured request timeout and response-byte cap, and must
return only the exact challenge marker. The authenticated gate proves both responses used
one candidate assignment, deletes its temporary session and cleanup journal,
and passes only salted hashes to the zero-traffic sticky gate. An interrupted
run recovers its private cleanup journal before creating another canary session.
Kernel leases serialize both the journal and authenticated handoff state. A
valid handoff is bound to the run and the routing-relevant config hash and
blocks a second authenticated coordinator for at most ten minutes; sticky holds
the state lease while consuming it. This prevents the process gap between the
two legs from letting another manifest overwrite or delete the pending handoff.

The failover gate first exercises the candidate's in-process failover logic
without external traffic. Live acceptance then requires a configured Codex
OAuth account whose valid credential and account-wide 100%-used window are
visible in candidate usage status. One forced no-retry request reconfirms that
already-unavailable account's structured quota rejection; an ordinary request
must succeed through a different account, and a final no-retry request must
reuse the replacement. The gate therefore costs two tiny successful provider
turns and one rejection against quota that was already exhausted. It never
manufactures exhaustion or edits the assignment ledger directly, and its
private cleanup journal makes an interrupted attempt recoverable.
The authenticated and failover journal leases are acquired before recovery and
held through final cleanup. A concurrent coordinator therefore fails without
touching the live journal or session; SIGKILL releases the kernel lease so the
next coordinator can validate and recover the stale exact run/session record.

The existing-session gate must observe the next turn of a session selected as
idle during the maintenance window. Candidate session inventory must explicitly
report that selected session as inactive; a missing activity field fails closed.
Before publishing its one-time challenge, the gate snapshots the configured
candidate proxy logs and sets a short future `not_before` boundary. The witness
command rejects an early response. Acceptance requires both an exact response
witness and a newly appended proxy-request record whose candidate log timestamp
is at or after that boundary for the selected agent/session and whose
`cutover_marker_hash` is the SHA-256 digest derived from the actual challenged
request body. The marker and request body are never logged. A kernel-held
challenge lease prevents a concurrent coordinator from replacing a live
challenge; after a crash or SIGKILL releases that lease, the next coordinator
can remove the stale artifacts and publish a replacement. Old or
pre-publication lines, changed files, partial or oversized appends, an unrelated
request on the selected session, a synthetic new session, or a second ephemeral
request are not continuity proof.

Activation records a single launchd identity snapshot plus a PID, executable
hash, start-time, and command fingerprint. A mode-`0700` transaction journal is
armed before the first live mutation. It waits for two observations of complete
label/PID/listener absence, atomically installs the candidate plist, and proves
that the candidate or its descendants are the sole listener owners. The
supervisor control socket must be a mode-`0600` Unix socket owned by the
expected uid and report one accepting, non-retiring backend. Candidate identity,
socket status, health, and readiness must remain stable through the functional
canary; before and after it, supervisor status must report the same live active
worker PID and kernel-bound CDHash. Bootstrap, structural acceptance, timeout,
signal, or canary failure
invokes the standalone rollback command automatically; a hard interruption is
recovered from the phase journal before a later activation may proceed.
Serving-store publication has its own requested and completed journal phases,
so recovery knows whether the candidate binding may have become visible before
the interruption.

The successful activation output prints the exact retained backup. To roll back
later, use that path and the installed supervisor path:

```bash
deploy/macos/rollback-launchagent-supervisor.sh \
  --backup '<printed-backup-path>' \
  --backup-sha256 '<printed-plist-sha256>' \
  --rollback-artifact '<rollback-program-destination>' '<bundle-artifact>' \
    '<printed-program-sha256>' '<mode>' \
  --expected-program "$HOME/bin/subrouter-supervisor"
```

Use the complete copy-pasteable command printed by successful activation; it
contains one `--rollback-artifact DEST ARTIFACT SHA MODE` entry for the rollback
program and each literal executable dependency discoverable from its plist or
shell wrapper. It also contains the serving-store path, exact candidate-binding
SHA-256, and either the exact prior binding artifact, SHA-256, and mode or an
exact-prior-absence declaration. The mode-`0700` bundle contains immutable
copies named by their SHA-256; activation also writes the same identities to
the printed mode-`0600` manifest beside the retained plist. Preserve the
complete bundle, not only the plist path.

Standalone rollback refuses a mismatched installed plist, loaded program, or
changed PID. Before requesting bootout, it verifies the retained plist and all
bundle artifacts against the activation hashes. The installed rollback-program
destinations may have received ordinary upgrades since activation; after exact
loaded-service identity and full launchd-label, captured-PID, and listener
absence are proven, rollback atomically restores the byte-checked artifact
copies over those destinations. It then requires the restored program identity,
health, and readiness. The script is host-neutral; customize
non-default installations with `SUBROUTER_LABEL`, `SUBROUTER_PLIST`,
`SUBROUTER_LAUNCHD_DOMAIN`, `SUBROUTER_HEALTH_URL`, and `SUBROUTER_READY_URL`.
`--expected-running-program` is reserved for transaction recovery when launchd
is still removing the captured legacy process; normal standalone rollback does
not need it.
If the installed plist is already absent, rollback proceeds only after proving
the launchd label and listener are absent, then restores the retained backup.

For current transaction bundles, rollback reconciles the serving-store binding
after proving complete candidate absence and before bootstrapping or probing the
legacy service. Under the shared serving-store lock, it may replace only the
exact recorded candidate binding with the exact recorded prior binding, or
remove it when prior state was absence. Finding the exact prior state is an
idempotent success; finding any third identity refuses rollback and retains the
journal instead of clobbering a concurrent operator change. Rollback commands
from older bundles that do not carry serving-store identity arguments leave the
binding untouched.

### Worker upgrade

Replace the worker binary atomically, then ask the stable supervisor to create a generation:

```bash
install -m 0755 ./subrouter /usr/local/bin/subrouter.new
mv -f /usr/local/bin/subrouter.new /usr/local/bin/subrouter
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock -X POST http://localhost/_subrouter/upgrade
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status | jq
```

The control socket lives in the service's state directory (`/var/lib/subrouter/supervisor.sock` for a `_subrouter` service user) because a non-root service cannot bind inside root-owned `/var/run`. The migration script writes the chosen path into the LaunchDaemon `--control-socket` argument, and `subrouter-autoupdate.sh` reads it back from the plist.

`deploy/macos/subrouter-autoupdate.sh` performs the same sequence with release checksum verification and automatic worker-binary rollback when readiness fails.

## Linux and GCP

The GCP VM runs the same supervisor. Migrate once, then every worker upgrade
preserves connections:

```bash
sudo deploy/gcp/migrate-systemd-to-supervisor.sh            # prepare and review
sudo deploy/gcp/migrate-systemd-to-supervisor.sh --activate # one-time listener transition
```

The prepared unit reuses the existing worker arguments, the existing `User=` and
`Group=`, and `HOME` from the running service. It deliberately omits
`StateDirectory=`, because systemd chowns that directory tree to the unit's user
and a supervised unit running as root would take `/var/lib/subrouter` away from
the unsupervised service it may need to roll back to.

Activation stops `subrouter.socket`, since socket activation and the supervisor
cannot both own the port. Rollback re-enables it, which matters because the
unsupervised unit declares `Requires=subrouter.socket` and will not start
without it.

`deploy/gcp/subrouter-autoupdate.sh` then upgrades through the control socket
and falls back to `systemctl restart` only when no supervisor is present.

### What socket activation does and does not preserve

Measured on the GCP VM:

| upgrade path | new connections | established connection | in-flight stream |
| --- | --- | --- | --- |
| `systemctl restart` (socket-activated) | accepted | closed by peer | cut |
| supervised worker upgrade | accepted | preserved | continued to completion |

Socket activation keeps the listener open, so clients are never refused. It does
not keep established connections alive: those descriptors belong to the worker
being replaced. For agent traffic, where one turn is one long streaming
response, that difference is a cancelled turn.

## Legacy unsupervised handoff

The following guarded path is only for installations that have not migrated yet. It still has a listener restart. New builds enter drain mode and wait for in-flight proxy requests on SIGTERM/SIGINT, but clients can see a connection gap because the worker owns the public listener.

On macOS, use the new binary's `install-daemon` path so launchd re-registers the LaunchAgent. Modern launchd can attach launch constraints to the binary it bootstrapped; a plain `mv` plus `launchctl kickstart -k` can fail with `OS_REASON_CODESIGNING | Launch Constraint Violation`.

Run from the Subrouter checkout:

```bash
set -euo pipefail

bin="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
label="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
service="gui/$(id -u)/$label"
health_url="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
backup="$bin.backup-$(date +%Y%m%d-%H%M%S)"
next="$(mktemp "$bin.next.XXXXXX")"
smoke_log="${TMPDIR:-/tmp}/subrouter-upgrade-smoke.log"

rollback() {
  if [ -x "$backup" ]; then
    "$backup" install-daemon \
      --addr 127.0.0.1:31415 \
      --cx-switch-interval 10m \
      --working-directory "$PWD"
  fi
}

go build -ldflags=-linkmode=external -o "$next" ./cmd/subrouter
chmod 0755 "$next"
if command -v codesign >/dev/null 2>&1; then
  codesign --force --sign - "$next" >/dev/null
  codesign --verify --verbose=4 "$next"
fi
"$next" help >/dev/null
curl -fsS "$health_url" >/dev/null

rm -f "$smoke_log"
"$next" serve --addr 127.0.0.1:31416 --fetch-usage=false --sr-switch-interval 0 >"$smoke_log" 2>&1 &
smoke_pid="$!"
smoke_ok=0
for _ in $(seq 1 40); do
  if curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null; then
    smoke_ok=1
    break
  fi
  sleep 0.25
done
kill "$smoke_pid" >/dev/null 2>&1 || true
wait "$smoke_pid" >/dev/null 2>&1 || true
if [ "$smoke_ok" != 1 ]; then
  cat "$smoke_log" >&2
  exit 1
fi

before_pid="$(launchctl print "$service" | awk '/pid =/ {print $3; exit}')"
before_sha="$(shasum -a 256 "$bin" | awk '{print $1}')"

cp -p "$bin" "$backup"
"$next" install-daemon \
  --addr 127.0.0.1:31415 \
  --sr-switch-interval 10m \
  --working-directory "$PWD"

ok=0
for _ in $(seq 1 80); do
  if curl -fsS "$health_url" >/tmp/subrouter-health.json; then
    ok=1
    break
  fi
  sleep 0.25
done

after_pid="$(launchctl print "$service" | awk '/pid =/ {print $3; exit}')"
after_sha="$(shasum -a 256 "$bin" | awk '{print $1}')"

printf 'before_pid=%s\nafter_pid=%s\nbefore_sha=%s\nafter_sha=%s\nbackup=%s\nhealth_ok=%s\n' \
  "${before_pid:-missing}" "${after_pid:-missing}" "$before_sha" "$after_sha" "$backup" "$ok"
cat /tmp/subrouter-health.json

if [ "$ok" != 1 ]; then
  rollback
  exit 1
fi
if [ "${before_pid:-}" = "${after_pid:-}" ]; then
  echo "subrouter pid did not change" >&2
  rollback
  exit 1
fi
```

Rollback uses the printed backup path:

```bash
set -euo pipefail

backup="<printed-backup-path>"

"$backup" install-daemon \
  --addr 127.0.0.1:31415 \
  --cx-switch-interval 10m \
  --working-directory "$(pwd)"
curl -fsS http://127.0.0.1:31415/_subrouter/health
```

## Rules

- Keep the public URL stable. Codex desktop and long-running CLI processes do not reliably adopt a new base URL mid-session.
- Check `/_subrouter/ready` before sending traffic to a process. A draining process returns 503.
- Use `POST /_subrouter/drain` from loopback before controlled shutdowns when you can.
- Use `install-daemon` for local macOS binary upgrades so launchd refreshes its launch constraint for the new binary.
- On Linux, install with `install-systemd` and keep `subrouter.socket` enabled. systemd owns the public TCP listener and passes it to the Subrouter worker, so new client connections queue across worker restarts instead of hitting a closed port.
- Do not edit the live binary in place. Build a separate binary, smoke-test it, then let `install-daemon` copy it into place.
- Do not use `kill -9`. Launchd owns restart policy and environment.
- Keep a backup binary until the new daemon has served real traffic.
- Do not work around upstream write failures by globally disabling connection pooling. Keep the outbound transport pooled, keep ChatGPT traffic on HTTP/1.1 to avoid HTTP/2 stream fanout, limit concurrent replayable `/responses` uploads, and handle transient `broken pipe`, `closed network connection`, `connection reset`, `unexpected EOF`, and TLS record errors with several replay attempts for buffered `/responses` and `/responses/compact` POSTs.

## Verifying compact failures

The daemon logs show request starts and proxy transport errors. A client-side compact error can be stale if the retry later completes, so check the transcript for the same session before changing code:

```bash
session_id="<codex-session-id>"
tail -n 50000 "$HOME/.subrouter/transcripts/by-agent/codex/by-session/$session_id.jsonl" \
  | jq -r 'select(.type=="subrouter_meta" or .type=="http_body") | [.timestamp,.type,(.payload.direction // "" ),(.payload.method // "" ),(.payload.path // "" ),(.payload.status // "" ),(.payload.bytes // "" ),(.payload.headers."Content-Length"[0] // "" ),(.payload.headers."Content-Encoding"[0] // "" )] | @tsv' \
  | tail -120
```

For a successful compact, expect a `subrouter_meta` row for `POST /responses/compact`, a full `client_to_upstream` body byte count matching `Content-Length`, then an `upstream_to_client` row with status `200`.

## Supervisor status

The permissioned Unix control socket reports the active generation and the connection count pinned to every draining generation. Browser pages cannot reach this socket or trigger upgrades:

```bash
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status | jq
```

There is intentionally no drain timeout. A routine upgrade never terminates a worker that still owns a client connection.
