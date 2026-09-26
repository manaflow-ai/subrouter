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

Anything that needs a real restart, including a plist change (`kickstart -k`
reuses the cached environment) or a new supervisor, goes through these instead.
Both hold the maintenance sentinel only for the operation, clear it on every
exit path including an interrupt, and run the stop/start as one detached
sequence that finishes even if the caller dies:

```bash
sudo subrouter-deploy.sh restart-daemon
sudo subrouter-deploy.sh install-supervisor /path/to/subrouter-supervisor
```

`install-supervisor` keeps the outgoing binary and puts it back if health does
not return.

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
only while that sentinel is fresh (`SUBROUTER_MAINTENANCE_MAX_AGE_MINS`, 90) —
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

## Recovering a host that is already down

```bash
sudo subrouter-deploy.sh rollback     # control-socket swap, listener stays up
```

If the service is restart-looping, the control socket is gone. Wait one minute
for the guard, or do it by hand: restore
`/var/lib/subrouter-verify/subrouter.last-good` over the binary, `bootout`,
confirm both pids are gone (`bootstrap` fails with `Input/output error` while
the old job drains under its 600s `ExitTimeOut`), then `bootstrap`.
