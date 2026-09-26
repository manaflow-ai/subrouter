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

Without `--label`, `/etc/subrouter-version` records `local:<sha>`, so
`subrouter-autoupdate.sh` replaces the build with the next release. Pass the
label of the release you are impersonating to keep a local build in place.

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
not return.

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
4. `sudo subrouter-deploy.sh install-supervisor /path/to/new-supervisor`. That
   restart picks up the plist and the new supervisor together.
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

## Recovering a host that is already down

```bash
sudo subrouter-deploy.sh rollback     # control-socket swap, listener stays up
```

If the service is restart-looping, the control socket is gone. Wait one minute
for the guard, or do it by hand: restore
`/var/lib/subrouter-verify/subrouter.last-good` over the binary, `bootout`,
confirm both pids are gone (`bootstrap` fails with `Input/output error` while
the old job drains under its 600s `ExitTimeOut`), then `bootstrap`.
