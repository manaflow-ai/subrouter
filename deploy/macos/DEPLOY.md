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
sudo subrouter-deploy.sh install /path/to/candidate --revision <full-commit> --label v0.1.130
sudo subrouter-deploy.sh status
```

`--revision` is the pushed commit the candidate was built from. The script
fetches this repository and refuses the candidate unless that commit contains
the commit recorded for the live worker. On 2026-09-22 a worker built from a
feature branch cut before the usage-sweep fixes replaced a worker that had
them; health stayed 200 while a third of the pool timed out on every sweep.
Build feature work on top of the live commit (`status` prints it), not on an
older branch. `--allow-unrelated "<reason>"` skips the check for an emergency,
and `record-revision <commit>` records the commit of a live worker that was
installed without one.

`install` refuses a candidate that does not answer `--help`, refuses to start
while health is already down, saves the serving binary as last-good, hot-swaps
through the control socket, and restores the previous binary by itself if the
candidate never becomes ready or public health drops.

Without `--label`, `/etc/subrouter-version` records `local:<sha>`, so
`subrouter-autoupdate.sh` replaces the build with the next release. Pass the
label of the release you are impersonating to keep a local build in place.

## Replace the supervisor without closing the port

```bash
sudo subrouter-deploy.sh handoff-supervisor /path/to/new-supervisor [--adopt-worker-config]
```

The supervisor owns the public listener, and macOS cannot pass a listener
between unrelated processes, so a restart used to close :31415 for about a
minute and cut every stream. `handoff-supervisor` moves the traffic with pf
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
plist while the bridge still serves. Rehearsed on cmux-lawrence on 2026-09-22
against a copy of the stack on a spare port: 0 failed probes out of 476 during
a handoff from the old supervisor, requests open across the switch completed,
and a crash-looping candidate was rolled back with 284 of 284 probes answered.
One probe in about 3300 failed during a long bridge phase under heavy
loopback churn; clients retry. The whole run logs to
`/var/log/subrouter-handoff.log`. If it stops with "the bridge still serves",
leave the anchor in place, bring the main job back, then load an empty
ruleset into the anchor (never `pfctl -F`, which drops translated states).

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
human clears `/Library/LaunchDaemons/<label>.plist.supervisor-transaction/upgrade-inhibited`.

`subrouter-verify.sh` (every 5 minutes) keeps the contract checks and defers
recovery whenever the guard heartbeat is fresh.

The guard stands down entirely while `subrouter-deploy.sh` holds its lock, for
up to five minutes: the deploy owns the outcome and reverts on its own, and a
guard tick inside that window would record the untested candidate as last-good.
For the same reason the deploy keeps its own private copy of the outgoing
binary and rolls back to that, never to the shared last-good file.

Both honor a `maintenance` sentinel younger than 90 minutes, with one
exception: if the service is not in the launchd domain at all, the guard
bootstraps it anyway once the sentinel is older than three minutes. A hand
restart passes through that state for seconds; an interrupted one leaves the
service there for good, and the sentinel must not make that permanent.

```bash
sudo tail -f /var/log/subrouter-guard.log
sudo tail -f /var/log/subrouter-verify.log
```

## Recovering a host that is already down

```bash
sudo subrouter-deploy.sh rollback     # control-socket swap, listener stays up
```

If the service is restart-looping, the control socket is gone. Wait one minute
for the guard, or do it by hand: restore
`/var/lib/subrouter-verify/subrouter.last-good` over the binary, `bootout`,
confirm both pids are gone (`bootstrap` fails with `Input/output error` while
the old job drains under its 600s `ExitTimeOut`), then `bootstrap`.
