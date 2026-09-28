# Deploying a worker to a supervised macOS host

The supervisor binds the public port only after the first worker answers
`/_subrouter/ready` inside `--ready-timeout` (30s by default). A restart
therefore turns a slow or broken worker into a total outage: `supervise` exits
before it binds, launchd restarts it every 30 seconds, and clients get
connection refused until someone puts the old binary back. That happened twice
on cmux-lawrence on 2026-09-04 and blocked every agent on the tailnet.

Replacing the binary and asking the supervisor to upgrade has no such failure
mode. The listener stays bound, the new generation starts behind it, and the
old generation keeps serving if the new one never becomes ready.

## Install a binary

```bash
sudo subrouter-deploy.sh install /path/to/candidate --label v0.1.130
sudo subrouter-deploy.sh status
```

`install` refuses a candidate that does not answer `--help`, refuses to start
while health is already down, saves the serving binary as last-good, hot-swaps
through the control socket, and restores the previous binary by itself if the
candidate never becomes ready or public health drops.

When the running supervisor answers `GET /_subrouter/canary`, `install`,
`install-release` and `subrouter-autoupdate.sh` send the new worker out as a
canary instead of a one-step upgrade and return once it has started (see
[Canary rollouts](#canary-rollouts-on-a-host)). A supervisor without the
endpoint gets the plain upgrade and the bake gate, as before. `install
--plain` or `SUBROUTER_DEPLOY_CANARY=0` forces the plain upgrade.

Without `--label`, `/etc/subrouter-version` records `local:<sha>`, so
`subrouter-autoupdate.sh` replaces the build with the next release. Pass the
label of the release you are impersonating to keep a local build in place.

## Move a host to main from your Mac

```bash
curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/macos/upgrade-host.sh | bash -s -- USER@HOST
```

Any ssh options go before the target (`-J jump -i key`). `upgrade-host.sh`
waits while the host is busy (deploy lock, maintenance, health down, load),
builds `cmd/subrouter` at `--ref` (default `main`) on the host, runs the
candidate's `codex isolation-check` against the live state, backs up the worker,
supervisor, plist, worker config, version file and service state to
`/var/lib/subrouter-verify/upgrade-backups/`, then hot-swaps with
`subrouter-deploy.sh install` and records the commit. With no pin in place it
pins autoupdate at the new build, since autoupdate would otherwise reinstall the
latest release. It watches loopback and tailnet health for two minutes, and on a
failure copies the old worker back and asks the supervisor for a new generation
directly (the listener stays bound). The host side runs under nohup, so a dropped
ssh session does not stop it. `--plan` builds, preflights and dry-runs the state
backup only. It needs passwordless sudo on the host and moves the worker only;
the supervisor moves only with `--enable-rollouts` (below). Log: `/var/log/subrouter-upgrade.log`.

## Adopt canary rollouts on a host (one command)

```bash
curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/macos/upgrade-host.sh \
  | bash -s -- --enable-rollouts -J browser-a -i ~/.ssh/id_ed25519_manaflow_cmux cmux-lawrence@172.20.21.158
```

`--enable-rollouts` goes before the ssh arguments. It builds `--ref` (default
`main`) and runs the same preflight and backup as a plain run, then:

1. installs that commit's `subrouter-deploy.sh`, `subrouter-guard.sh`,
   `subrouter-verify.sh`, `subrouter-autoupdate.sh`, `release-bake-lib.sh`,
   `mutation-lease-lib.sh` and `subrouter-supervisor-handoff.sh` into
   `/usr/local/bin`, together under `deploy.lock`, keeping the replaced copies
   in `upgrade-backups/<timestamp>/scripts`;
2. hands the supervisor off to the build with `subrouter-deploy.sh
   handoff-supervisor --adopt-worker-config` (below), so the port never
   closes, unless the supervisor already is that build and the plist already
   passes `--worker-config`;
3. adds `SUBROUTER_RELEASE_STATE=/var/lib/subrouter-verify/release-state.json`
   to the worker config's `env` with `subrouter-deploy.sh reconfigure`, a hot
   upgrade, unless it is already there;
4. leaves autoupdate off. An existing pin stays as it is; with none it writes
   one, before step 1, so the new autoupdate cannot start a canary that the
   reconfigure in step 3 would supersede. Step 3 refuses while a canary is
   pending. It prints the command that turns autoupdate on later
   (`sudo subrouter-deploy.sh unpin`, once releases are cut on green main,
   RFC #444 step B), and says so when no LaunchDaemon runs
   `subrouter-autoupdate.sh` yet.

It does not move the worker. `--plan` prints the three steps it would take and
changes nothing; a second run is a no-op. The plain path of `upgrade-host.sh`
keeps working before and after: afterwards its `subrouter-deploy.sh install`
goes out as a canary, and its watch reports a canary the gate aborted.

## Replace the supervisor without closing the port

```bash
sudo subrouter-deploy.sh handoff-supervisor /path/to/new-supervisor [--adopt-worker-config]
```

The supervisor owns the public listener, and macOS cannot pass a listener
between unrelated processes, so a restart closes :31415 for about a minute and
cuts every stream. `handoff-supervisor` (from #353) moves the traffic with pf
instead:

1. A bridge supervisor (`<label>.handoff`, the candidate binary, the live
   worker copied to `subrouter-handoff`) starts on the next port.
2. An rdr anchor (`com.apple/subrouter-handoff`) sends new connections for the
   public port on loopback and tailnet addresses to the bridge. Established
   connections keep their pf state and stay on the old supervisor. This relies
   on the `keep state` pass rules in the `ai.manaflow.subrouter` anchor.
3. The old job is booted out and drains its own streams (up to 10 minutes).
4. The job is bootstrapped with the new binary and plist and probed through a
   reserved source-port exception (45990-45999).
5. The redirect is dropped, and the bridge drains and is removed.

A candidate that never becomes ready is replaced with the backed-up binary and
plist while the bridge still serves. `--adopt-worker-config` also wires
`--worker-config` into the plist, writing the file from the plist's worker
args if it is missing. Rehearsed on cmux-lawrence on 2026-09-22 against a copy
of the stack on a spare port: 0 failed probes out of 476 during a handoff, and
a crash-looping candidate was rolled back with 284 of 284 probes answered. The
run logs to `/var/log/subrouter-handoff.log`. If it stops with "the bridge
still serves", leave the anchor in place, bring the main job back, then load
an empty ruleset into the anchor (never `pfctl -F`, which drops translated
states). It refuses to run while a canary rollout is pending, because the new
supervisor starts its worker from the binary on disk, which is the candidate.

## Install a release, pin it, roll it back

```bash
sudo subrouter-deploy.sh install-release v0.1.140
sudo subrouter-deploy.sh pin v0.1.139      # installs v0.1.139 first if it is not running
sudo subrouter-deploy.sh unpin
sudo subrouter-deploy.sh list
sudo subrouter-deploy.sh rollback --to v0.1.139
```

`install-release` downloads `subrouter_<version>_darwin_<arch>` and the
release `SHA256SUMS`, requires exactly one matching checksum line and a
matching digest (the same check `subrouter-autoupdate.sh` makes), then runs
`install --label <version>`. Nothing is installed on a checksum failure.

`pin` writes the autoupdate inhibit sentinel
(`/Library/LaunchDaemons/<label>.plist.supervisor-transaction/upgrade-inhibited`)
with `pinned at <version> by ...`; `subrouter-autoupdate.sh` prints that line
when it defers. With a version, the pin is written before the install, so
autoupdate cannot slip in between, and a failed install leaves the host pinned
at the release that is still running. `unpin` removes the sentinel, including
the one the guard writes after an automatic rollback. `sr doctor` on the host
reports the pin.

Every `install`, `install-release`, `rollback` and autoupdate keeps the worker
it replaced in `/var/lib/subrouter-verify/backups/<epoch-ns>_<version>`, and
only the newest three are kept. Older loose `/usr/local/bin/subrouter.backup-*`
and `.rejected-*` copies are pruned to three as well. `list` prints them;
`rollback --to <version>` puts one back and records that version, while plain
`rollback` restores the guard's last-good and records `rollback:<sha>`. A
rollback does not pin: run `pin` afterwards or autoupdate reinstalls the
latest release on its next run.

## Never do this

```bash
sudo cp candidate /usr/local/bin/subrouter          # no rollback, no staging
sudo launchctl bootout system/ai.manaflow.subrouter-team   # closes the port
sudo launchctl bootstrap system /Library/LaunchDaemons/ai.manaflow.subrouter-team.plist
```

Typing that sequence by hand is how the router was left down on 2026-09-04: the
ssh session running it was interrupted between the bootout and the bootstrap,
so the service stayed out of the launchd domain with the port closed, and the
maintenance sentinel the operator had set kept the watchdog from healing it.

Worker flag and environment changes do not need a restart: use `reconfigure`
(below). Anything that needs a real restart, meaning a supervisor flag change
in the plist (`kickstart -k` reuses the cached environment) or a new supervisor, goes through these instead.
Both hold the maintenance sentinel only for the operation, clear it on every
exit path including an interrupt, and run the stop/start as one detached
sequence that finishes even if the caller dies:

```bash
sudo subrouter-deploy.sh restart-daemon
sudo subrouter-deploy.sh install-supervisor /path/to/subrouter-supervisor
```

`install-supervisor` keeps the outgoing binary and puts it back if health does
not return. Prefer `handoff-supervisor` (above), which never closes the port.

## Change worker flags or environment

Worker flags and worker environment live in a JSON file that the supervisor
re-reads for every worker generation, so changing them is a hot upgrade behind
the bound listener. Do not edit the plist for them. On 2026-09-22 a plist edit
to add Bedrock flags needed a `bootout` and `bootstrap`, and every client got
connection refused for about a minute.

```bash
sudo subrouter-deploy.sh reconfigure /path/to/worker-config.json
```

The file is `{"args": ["--flag", "value"], "env": {"KEY": "value"}}`. `args`
replaces the worker args after `--` in the plist, and `env` overrides the
supervisor environment for the worker only. `--addr`, `--local-data-socket`,
`SUBROUTER_LISTEN_FD`, and `SUBROUTER_PRIVATE_DATA_ROUTER` are owned by the
supervisor and are refused. `reconfigure` validates the file, keeps the live
owner and mode, backs up the live file, and restores it and upgrades back if
the new worker never becomes ready or public health drops. A replacement
generation with an unreadable file is refused and the old worker keeps
serving. An initial generation falls back to the plist worker args, so a bad
file cannot keep the port closed after a restart.

The plist now changes only for supervisor flags or a new supervisor, through
`restart-daemon` or `install-supervisor`.

### One-time adoption on a host

The running supervisor must understand `--worker-config` and the plist must
pass it. This is the last planned restart for worker changes.

1. Build the supervisor from this branch and copy it to the host.
2. Write `/var/lib/subrouter/worker-config.json` from the current plist: the
   worker args after `--` go in `args`, and the worker-only entries of
   `EnvironmentVariables` go in `env`. Keep the plist values in place as the
   fallback. Make the file `_subrouter:_subrouter` mode `0600`.
3. Add `--worker-config /var/lib/subrouter/worker-config.json` to the plist
   before `--`.
4. `sudo subrouter-deploy.sh handoff-supervisor /path/to/new-supervisor
   --adopt-worker-config`, which also does steps 2 and 3 and picks up the
   plist and the new supervisor together without closing the port.
   `upgrade-host.sh --enable-rollouts` runs it for you.
5. Prove the path: `sudo subrouter-deploy.sh reconfigure` with the same file
   plus a harmless change, and confirm the listener never drops.

## Listen address

Use `--addr :31415`, not `--addr 0.0.0.0:31415`. The IPv4 wildcard binds IPv4
only, which is exactly what it means, and current supervisors honour it. Older
builds bound dual stack from the same string, so upgrading a supervisor on a
host whose clients arrive over IPv6, such as anything on the tailnet, silently
drops those clients. That happened on cmux-lawrence on 2026-09-04. The
supervisor now logs the family it bound and warns when it is IPv4 only.

```bash
curl -m 5 "http://[$(tailscale ip -6 <host> | head -1)]:31415/_subrouter/health"
```

## Watchdogs

`subrouter-guard.sh` (`ai.manaflow.subrouter-guard`, every 60s) records the
binary that is serving traffic as last-good, and on two consecutive failed
health probes restores it and restarts the service. That bounds a bad-worker
outage at about two minutes. A rollback also writes the autoupdate inhibit
sentinel, so a bad release cannot flap: worker updates stay paused until a
human clears `/Library/LaunchDaemons/<label>.plist.supervisor-transaction/upgrade-inhibited`
(`sudo subrouter-deploy.sh unpin`).
It also rewrites `/etc/subrouter-version` to `rollback:<sha> (was <release>)`,
so verify reports what is actually running and autoupdate retries the
release once the sentinel is cleared.

`subrouter-verify.sh` (every 5 minutes) keeps the contract checks and defers
recovery whenever the guard heartbeat is fresh.

`/var/lib/subrouter-verify/deploy.lock` is the one lock for every writer of
the worker binary. `subrouter-deploy.sh` holds it for the whole operation,
`subrouter-autoupdate.sh` from just before its swap until health confirms the
new generation, and the guard for each tick; `deploy.lock/owner` names the
holder. The guard stands down while someone else holds it, for up to five
minutes: the deploy or updater owns the outcome and reverts on its own, and a
guard tick inside that window would record the untested candidate as
last-good. Autoupdate defers to the next run while the lock is held, and the
deploy script waits up to 90 seconds for it. For the same reason the deploy
keeps its own private copy of the outgoing binary and rolls back to that,
never to the shared last-good file.

Both honor a `maintenance` sentinel younger than 90 minutes, with one
exception: if the service is not in the launchd domain at all, the guard
bootstraps it anyway once the sentinel is older than three minutes. A hand
restart passes through that state for seconds; an interrupted one leaves the
service there for good, and the sentinel must not make that permanent.

```bash
sudo tail -f /var/log/subrouter-guard.log
sudo tail -f /var/log/subrouter-verify.log
```

## Log rotation

`install-daemon` writes `StandardOutPath`/`StandardErrorPath` into the service
plist and nothing prunes them. A busy pool logs one INFO line per upstream
request, so those files reach hundreds of megabytes within days and then grow
until the disk fills.

`subrouter-log-rotate.sh` (`ai.manaflow.subrouter-log-rotate`, every 15 minutes)
reads the log paths back out of the installed plists rather than hardcoding
them, so it rotates whatever the service is actually configured to write, and
compresses anything over `SUBROUTER_LOG_MAX_BYTES` (64 MiB).

It copies the contents out and truncates the file **in place** rather than
renaming it. launchd opens those paths once and holds the descriptor for the
life of the job: renaming moves the name but not the inode, so the service
would keep writing into the rotated file while the new one stayed empty
forever. Truncation keeps the inode, and therefore launchd's descriptor, so no
restart and no signal is needed. The trade is that lines written between the
copy and the truncate are lost, which is why rotation triggers on size and so
happens rarely. `newsyslog` remains the right tool for logs that no
long-running process holds open.

Retention is bounded twice, whichever comes first: at most
`SUBROUTER_LOG_KEEP` (5) compressed generations, and nothing older than
`SUBROUTER_LOG_MAX_AGE_DAYS` (14; `0` disables the age bound). A count alone
keeps a quiet host's archives forever; an age alone lets a busy pool keep
hundreds of files inside the window.

Like the other jobs it stands down for a `maintenance` sentinel, and like them
only while that sentinel is fresh (`SUBROUTER_MAINTENANCE_MAX_AGE_MINS`, 90), since
a forgotten sentinel must not be able to fill the disk. It also rotates its own
log and leaves `/dev/null` alone.

The job runs as root but reads plists from every user's
`~/Library/LaunchAgents`, so a log path can sit in a directory another user
controls. A log owned by a user is rotated with that user's privileges
(`sudo -u`), so nothing the job does there can reach a file the user could not
already change. A root-owned log is rotated only when every directory above it
is root-owned and writable by nobody else. The log is opened without following
symlinks, a hard-linked log is refused, the archive is created exclusively so a
planted name cannot redirect it, and the same open descriptor is archived and
truncated.

```bash
sudo install -m 0755 deploy/macos/subrouter-log-rotate.sh /usr/local/bin/
sudo install -m 0644 deploy/macos/ai.manaflow.subrouter-log-rotate.plist /Library/LaunchDaemons/
sudo launchctl bootstrap system /Library/LaunchDaemons/ai.manaflow.subrouter-log-rotate.plist
deploy/macos/tests/log-rotate-test.sh
```

## Bake gate

A release that becomes ready and answers health can still break routing or
streaming, and nothing above catches that. So every worker installed by
`subrouter-deploy.sh install`/`install-release` or `subrouter-autoupdate.sh`
bakes before it becomes last-good:

1. Before the swap the installer reads the outgoing generation's
   `/_subrouter/traffic` (request and outcome counts since that worker
   started) as the baseline. After the swap it writes
   `/var/lib/subrouter-verify/release-state.json` with `state: "baking"` and
   `bake_until = now + SUBROUTER_BAKE_SECONDS` (default 1200).
2. Each guard tick compares the new worker's ratios per request with the
   baseline's: subrouter-generated 5xx (`proxy_5xx`: no usable account,
   upstream dial failure, internal error), proxy-side stream drops, and all
   5xx. A ratio trips only with at least `SUBROUTER_BAKE_MIN_REQUESTS` (50)
   requests and `SUBROUTER_BAKE_MIN_ERRORS` (5) failures of that kind, and only
   when it is above both `SUBROUTER_BAKE_RATIO_FACTOR` (2) x baseline and
   baseline + margin (`SUBROUTER_BAKE_PROXY_5XX_MARGIN` 0.02,
   `SUBROUTER_BAKE_STREAM_DROP_MARGIN` 0.02, `SUBROUTER_BAKE_5XX_MARGIN`
   0.05). `SUBROUTER_BAKE_MAX_RESTARTS` (2) worker restarts during the bake
   also trip it. The last-good file does not move while a worker bakes.
3. On a trip the guard puts last-good back through the supervisor's hot
   upgrade (the listener stays up), writes `state: "rolled_back"` with the
   reason, sets `/etc/subrouter-version` to the previous release, and pins
   autoupdate there. `sudo subrouter-deploy.sh unpin` resumes it.
4. When the window passes without a trip the guard records the worker as
   last-good and writes `state: "promoted"`. Low traffic cannot trip a ratio,
   so a quiet bake is promoted on health alone, and the reason says so.

```bash
sudo subrouter-deploy.sh status     # live, last-good, pin, and the bake with its baseline
sudo subrouter-deploy.sh promote    # end a bake early
sudo subrouter-deploy.sh unpin      # after reviewing a rollback
```

`sr status` against the team server and `sr doctor` on the host print one
`release` line from `/_subrouter/health`, e.g. `v0.1.150 baking (12m left)` or
`rolled back from v0.1.150 to v0.1.149: proxy 5xx 4.1% vs 0.2% baseline`.
The worker reports that field only when `SUBROUTER_RELEASE_STATE` (or
`--release-state`) names the file. `migrate-launchdaemon-to-supervisor.sh`
sets it in the plist; on a host migrated earlier add
`"SUBROUTER_RELEASE_STATE": "/var/lib/subrouter-verify/release-state.json"`
to the worker config's `env` and run `subrouter-deploy.sh reconfigure`, which
needs no restart.

The gate needs `release-bake-lib.sh` next to the scripts
(`sudo install -m 0755 deploy/macos/release-bake-lib.sh /usr/local/bin/`).
Without it every script keeps its old behavior. `SUBROUTER_BAKE_SECONDS=0`
turns the gate off. A worker that predates `/_subrouter/traffic` gives no
counts, so its bake checks health and restarts only.

## Canary rollout (supervisor)

Instead of `/_subrouter/upgrade`, the supervisor can run the new worker as a
candidate beside the incumbent and give it a weighted share of new sessions
(RFC #444, step A). A session (the Claude or Codex session id in the request
head) stays on the generation it first reached; a connection without one is
split on its own; no open connection ever moves. Pinning is per connection:
the generation is chosen from a connection's first request, so a keep-alive
connection that later carries other sessions takes them to that same
generation, even sessions pinned to the other one. The candidate runs the binary
at `--worker-bin`, so install it first, exactly as for an upgrade.

```bash
SOCK=/var/run/subrouter-supervisor.sock   # the plist's --control-socket
c() { sudo curl -fsS --unix-socket "$SOCK" "$@"; }
c -X POST 'http://localhost/_subrouter/canary/start?steps=default'  # 5% -> 25% -> 100%, 10m each, gated
c -X POST 'http://localhost/_subrouter/canary/start?steps=5,50,100&dwell=20m'
c -X POST 'http://localhost/_subrouter/canary/start?weight=5'       # manual: no stepper
c -X POST 'http://localhost/_subrouter/canary/weight?weight=25'     # takes over from the stepper
c -X POST 'http://localhost/_subrouter/canary/promote'              # sole generation; drain the old one
c -X POST 'http://localhost/_subrouter/canary/abort?reason=why'     # weight 0; drain and stop the candidate
c http://localhost/_subrouter/canary                                # state, gate, both generations' traffic
```

With `steps`, the supervisor checks a gate every dwell/10 (at most 30s). It
compares the candidate with the incumbent over the same window since the
candidate started, using the bake gate's thresholds and `SUBROUTER_BAKE_*`
overrides from the supervisor's environment. A regression aborts at once;
otherwise each step promotes after its dwell, and the last one makes the
candidate the sole generation. Low traffic promotes on health alone. The
bake's restart rule has no counterpart: a candidate that exits aborts the
rollout at once. Setting a weight, promoting or aborting by hand stops the
stepper, and a gate check it had in flight is discarded. An inhibit marker
blocks start and promote, never abort.

A plain `/_subrouter/upgrade` keeps its meaning (one new generation from the
binary on disk). During a rollout it aborts the canary first, so the deploy
scripts and the guard work unchanged.

The rollout is written to the release-state file (`--release-state`, else
`SUBROUTER_RELEASE_STATE` from the supervisor or worker config env) with state
`canary`, `promoted` or `aborted` and a `weight`, so `/_subrouter/health`,
`sr status` and `sr doctor` show e.g. `v0.1.141 canary 25% (9m)` or
`v0.1.141 aborted at 5%: proxy 5xx 3.2% vs 0.3% ...`. The guard's bake acts
only on `baking`, so it ignores these. After an abort the candidate binary is
still at `--worker-bin`; the host scripts below put last-good back.

## Canary rollouts on a host

The host scripts drive the supervisor canary (RFC #444 step C):

- `subrouter-deploy.sh install`/`install-release` and `subrouter-autoupdate.sh`
  probe `GET /_subrouter/canary`. When it answers, they save the serving
  worker as last-good (it keeps serving through the rollout), install the
  candidate at the worker path, `POST /_subrouter/canary/start?steps=default`,
  write `/etc/subrouter-version`, and return. `SUBROUTER_CANARY_STEPS` and
  `SUBROUTER_CANARY_DWELL` override the steps and dwell. A refused start (a
  candidate that never becomes ready) puts the previous binary back; the
  incumbent never stopped serving. A worker that is still baking from a plain
  install is replaced with a plain upgrade, and an install while a canary is
  pending is refused (autoupdate defers).
- The rollout is recorded in `/var/lib/subrouter-verify/canary-rollout.json`:
  label, previous label, candidate and incumbent digests, the candidate
  generation id, and `resolved` once the outcome is handled. The canary
  replaces the bake: no `baking` state is written, and while the rollout is
  pending the guard never records the binary on disk as last-good.
- Whoever holds `deploy.lock` and notices the end first handles it: the guard
  on its tick, or a later `install`, `promote`, `abort` or autoupdate run.
  - `promoted`: the candidate becomes last-good, as a passed bake does.
  - `aborted`: last-good goes back at the worker path and the supervisor
    starts a generation from it through the control socket, with no restart
    (the listener stays bound, and when the incumbent already ran last-good
    this only replaces it with the same binary), autoupdate is pinned with
    `pinned at <previous> by the canary gate: aborted <version> at <weight>%
    ... (<reason>)`, and `/etc/subrouter-version` names the previous version
    again. `sudo subrouter-deploy.sh unpin` resumes autoupdate after review.
  - A supervisor that has no record of the rollout (it restarted, and its new
    worker came from the candidate binary) is treated as aborted. The same
    goes for a generation that runs the candidate after the canary ended
    another way: a plain upgrade superseded it, or the incumbent crashed and
    was replaced from `--worker-bin` (the supervisor then aborts the rollout).
    The hot swap to last-good above covers all of these.
- Health probes carry no session key, so at 25% or 100% they can reach a hung
  candidate. When health has failed for the guard's strike threshold (two
  checks) during a canary, the guard aborts the canary before any restart and
  takes the outage path only if health is still down on the incumbent. One
  transient failure is a strike, never an abort or a pin. The supervisor's gate also aborts a candidate
  that fails three consecutive `/_subrouter/ready` checks.
- `reconfigure` is a plain upgrade, so it is refused while a canary is
  pending, as are `handoff-supervisor` and `install-supervisor`. `install`
  refuses when a rollout is pending and the supervisor does not answer.

```bash
sudo subrouter-deploy.sh status          # the canary line: weight, step, gate; and the rollout record
sudo subrouter-deploy.sh promote         # promote the candidate to all traffic now
sudo subrouter-deploy.sh abort "reason"  # abort it now; last-good goes back and autoupdate is pinned
```

`sr status` against the team server prints the rollout as its `Release` line
(for example `main-0123abcd4567 canary 25% (9m)`) once the worker has
`SUBROUTER_RELEASE_STATE`, which `upgrade-host.sh --enable-rollouts` sets.

## Recovering a host that is already down

```bash
sudo subrouter-deploy.sh rollback     # control-socket swap, listener stays up
```

If the service is restart-looping, the control socket is gone. Wait one minute
for the guard, or do it by hand: restore
`/var/lib/subrouter-verify/subrouter.last-good` over the binary, `bootout`,
confirm both pids are gone (`bootstrap` fails with `Input/output error` while
the old job drains under its 600s `ExitTimeOut`), then `bootstrap`.
