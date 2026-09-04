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

## Never do this

```bash
sudo cp candidate /usr/local/bin/subrouter          # no rollback, no staging
sudo launchctl bootout system/ai.manaflow.subrouter-team   # closes the port
sudo launchctl bootstrap system /Library/LaunchDaemons/ai.manaflow.subrouter-team.plist
```

A `bootout` is only correct when the plist itself changed, because `kickstart
-k` reuses the cached environment. In that case set the maintenance sentinel
first so the watchdogs do not fight the restart, and clear it after:

```bash
sudo touch /var/lib/subrouter-verify/maintenance
# bootout, confirm the supervisor and worker pids are gone, bootstrap
sudo rm /var/lib/subrouter-verify/maintenance
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

Both honor a `maintenance` sentinel younger than 90 minutes.

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
