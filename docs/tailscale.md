# Serving Subrouter over a tailnet

One machine runs the daemon and holds the accounts. Every other machine on the tailnet points its agents at that daemon instead of running its own. This is the shape to use when a pool of provider accounts should be shared by a few people, or by one person across a laptop, a desktop, and a VM, without copying provider credentials onto each of them.

The daemon it installs is the same `subrouter serve` that a loopback install runs. The only difference is the bind address and the admin token that a non-loopback bind requires.

## The tailnet is not the security boundary

Tailscale decides which hosts can *reach* the port. It does not decide what they can *do* once they reach it, and Subrouter does not treat tailnet membership as authorization.

`authorizeAdmin` trusts exactly one thing implicitly: a loopback peer. Every other caller — including one that is unambiguously on your tailnet, with a valid node key, admitted by your ACLs — must present the admin token to touch `/_subrouter/accounts`, `/_subrouter/transcripts`, `/_subrouter/account-import`, or `/_subrouter/drain`.

That is deliberate, and it is a repaired bug rather than an original design. Tokenless remote admin used to be the silent default: a daemon bound to a tailnet address with no `--admin-token` handed every reachable host the full admin surface, including transcripts and account import. So `serve` now refuses to start on a non-loopback address without a token, and as of this change `install-daemon` refuses to install one. There is an `--allow-unauthenticated-admin` escape hatch on `serve`; treat it as a debugging tool, not a deployment option.

Concretely: your ACLs are the second lock, not the first. A tailnet with a permissive `autogroup:member` rule is a normal, sensible tailnet and is still not sufficient on its own.

## Install on macOS

Generate a token and put it in its own file. Prefer this form — the secret then never enters the plist at all.

```bash
mkdir -p ~/.subrouter
openssl rand -hex 32 > ~/.subrouter/admin-token
chmod 600 ~/.subrouter/admin-token

make build
./bin/subrouter install-daemon \
  --addr 100.x.y.z:31415 \
  --admin-token-file ~/.subrouter/admin-token
```

Bind the **tailnet IP**, not the MagicDNS name. The address is resolved once, at bind time, and a LaunchAgent can start before `tailscaled` has MagicDNS answering; the 100.x address on the interface is ready earlier and does not change. MagicDNS names are the right thing for *clients* to dial, just not for the listener.

The alternative form puts the token inline in the plist's `EnvironmentVariables`, and the installer then writes the plist `0600` instead of `0644`:

```bash
./bin/subrouter install-daemon --addr 100.x.y.z:31415 --admin-token "$TOKEN"
```

Pass one or the other, never both — `serve` treats `SUBROUTER_ADMIN_TOKEN` and `SUBROUTER_ADMIN_TOKEN_FILE` both being set as a fatal misconfiguration, so accepting both flags would install a daemon that validates cleanly and then fails at boot.

Either way the token stays out of `ProgramArguments`. launchd argv is world-readable through `ps`, so a token passed there would be legible to every local user on the machine — the exact audience the token exists to gate.

## Install on Linux

`install-systemd` has taken `--admin-token` for a while and is unchanged:

```bash
sudo sr install-systemd --addr 100.x.y.z:31415 --admin-token-stdin <<<"$TOKEN"
```

`--admin-token-stdin` is the systemd equivalent of `--admin-token-file`: it keeps the secret out of the process arguments and out of your shell history. There is no `--admin-token-stdin` on macOS; use `--admin-token-file`.

## Point the clients at it

On each other machine:

```bash
sr server add pool --url http://host.tailnet-name.ts.net:31415 --admin-token "$TOKEN" --default
```

MagicDNS is fine here. The token is the same one the daemon was installed with; a client without it can still proxy provider traffic but cannot read accounts, sessions, the dashboard, or transcripts.

Account onboarding is a separate, narrower credential: `SUBROUTER_ACCOUNT_IMPORT_TOKEN` authorizes `GET` and `POST /_subrouter/account-import` and nothing else. Give that to a machine that only needs to hand accounts in, and it cannot read admin APIs or proxy traffic with it.

## What remains loopback-only

`/_subrouter/drain` is refused from any non-loopback peer regardless of token. Draining the pool is something you do on the host itself, over SSH or at the keyboard — a correct admin token does not buy it remotely.

`/_subrouter/health` stays unauthenticated in both directions so liveness probes work. `/_subrouter/ready` likewise, returning 503 while draining.

## Operating it

Logs are `~/Library/Logs/subrouter.log` and `~/Library/Logs/subrouter.err.log`. The plist sets `KeepAlive`, so a bind that loses the race with `tailscaled` at login does not leave the daemon dead — launchd restarts it until the address is available. The tell is a burst of start/exit pairs in the error log that stops on its own. A burst that *doesn't* stop is a real bind failure: wrong IP, or the Tailscale node changed address.

Rotating the token means rewriting the token file (or reinstalling with a new `--admin-token`), restarting the daemon, and running `sr server add` again on every client with the new value. Clients fail closed and loudly on a stale token — a 401 on the admin endpoints, not a silent downgrade to unauthenticated.

## Before you expose one

[docs/production.md](production.md) is the checklist — listener, tokens, credential transfer, drain behaviour. This document covers the topology and the install; that one covers whether the result is fit to leave running.
