# subrouter

Go service for routing AI coding-agent traffic across subscription accounts and API keys.

## Development

- Use `go test ./...` before handing off changes.
- Keep credential handling read-only unless a command explicitly delegates to the upstream account manager, such as `cx`.
- Do not log access tokens, refresh tokens, API keys, request bodies, or complete Authorization headers.
- Claude web `sessionKey` cookies (read from local browsers to show the prepaid extra-usage balance in `sr status`) follow the same rule: read-only, never logged, persisted only at `claude-web-sessions.json` under `storepath.CodexDir()` with mode 0600, and used only by the local CLI — never sent to the server or worker. Reading Chromium cookies may trigger a one-time macOS Keychain prompt for the browser's "Safe Storage" entry.
- Prefer standard-library networking primitives unless a dependency removes meaningful complexity.

## The deliverable is a command Lawrence can run on his Mac

Work is not done when the server is correct. It is done when there is a command
he can paste into his own terminal, on his own machine, and see the thing work.

Every handoff states, in this order:

1. The exact command(s) to run locally. No `ssh`, no `gcloud compute ssh`, no
   admin token, no "first export this". If onboarding needs those, onboarding is
   the bug.
2. What he should see when it works.
3. Anything that is *not* runnable by him, named explicitly, with the reason.

Before claiming something works, run it the way he would: from a client machine,
through the public endpoint, as a user without privileged credentials. Verifying
from inside the VM, over SSH, or with an admin token proves the server works and
says nothing about whether anyone can use it. Those are different claims and
only the second one is the deliverable.

An interactive OAuth login is the one thing an agent genuinely cannot complete.
When a change depends on it, say so up front rather than at the end of a long
report, because that sentence is the difference between him testing it now and
him discovering he cannot after reading everything else.

If a change cannot be exercised from his machine at all, say that plainly in the
first line of the handoff. "Deployed and verified server-side, not yet testable
by you, because X" is an acceptable status. Implying it is ready when it is not
wastes the one thing he cannot get back.

## Never report a green suite you did not actually see

`main` was broken by a change whose test run was reported as passing. The
command was:

```
go test ./... 2>&1 | grep -vE '^ok|no test files' | head -5; echo SUITE_OK
```

Three defects, all in the verification rather than the code. A pipeline's exit
status is its last command, so `head` masked the failure. `echo` was joined with
`;` rather than `&&`, so the success marker printed unconditionally. And
`head -5` was consumed by unrelated install output, so the `--- FAIL` line was
never displayed.

So: never print a success marker that is not gated on the exit status, never
truncate test output in a way that can hide a failure line, and grep *for*
`FAIL` rather than filtering `ok` away. Prefer:

```
go test ./... > /tmp/test.log 2>&1 && echo PASS || { echo FAIL; grep -E '^(---|FAIL|panic)' /tmp/test.log | head -20; }
```

## Never merge before the run finishes

The same change reached `main` because it was merged while its CI run was still
going, and the next PR was merged on top of a run that had already failed. A
local pass is not a merge signal, and a queued run is not a passing run.

`main` merges through a merge queue. The required checks are `Build, vet, test`
and `CLA Assistant v3`, and they must pass twice: once on the pull request head
to enter the queue, and again on the merge group commit, which is the pull
request applied on top of `main` and every entry ahead of it in the queue. The
branch does not need to be up to date with `main`, so do not merge `main` into
it just because other pull requests landed. The queue tests against current
`main` for you. Update the branch only for a real conflict or for a failure
that comes from `main`.

Wait for the pull request's whole run to complete and pass, then queue exactly
the head that passed:

```
gh pr checks <PR> --repo manaflow-ai/subrouter --watch --fail-fast
gh pr merge <PR> --repo manaflow-ai/subrouter --match-head-commit "$(gh pr view <PR> --repo manaflow-ai/subrouter --json headRefOid --jq .headRefOid)"
```

`gh pr merge` only adds the pull request to the queue, and the queue picks the
merge method (squash). Being queued is not being merged. The pull request
merges when its merge group passes. GitHub removes it from the queue if the
group fails, if the queue times out, or if you push to the branch. Watch it
until `state` is `MERGED`:

```
gh pr view <PR> --repo manaflow-ai/subrouter --json state,mergedAt
```

Where `glaeda-gh` is installed, wait with it instead of polling `gh`. Every
session on a machine shares one GitHub API quota, and `glaeda-gh` is the one
poller that serves all of them. The first command exits 1 at the first failed
check. The second exits 0 once the pull request merges, but a pull request the
queue drops stays open, so it waits until `--timeout`; read the merge group run
if it has not merged by then:

```
glaeda-gh wait pr manaflow-ai/subrouter#<PR> --sha "$(git rev-parse HEAD)"
glaeda-gh wait pr manaflow-ai/subrouter#<PR> --until merged
```

If it drops back to `OPEN`, read the failed merge group run
(`gh run list --repo manaflow-ai/subrouter --event merge_group`), fix the
cause, and queue it again. Never bypass the queue with `--admin`.

When a branch does need `main`, merge it locally:

```
git fetch origin && git merge origin/main && git push
```

Do not use `gh pr update-branch` or the "Update branch" button. The merge
commit they create is committed by `GitHub <noreply@github.com>`, and the CLA
check treats that committer as a contributor who has not signed, so
`CLA Assistant v3` fails.

If CI is red on `main`, fixing it comes before any other work, including work
that was already in progress.

## Every commit author and committer must be a signed human

The CLA check treats every author, co-author, and committer on a pull
request's commits as a contributor who must sign. A bot address can never
sign, so any of these makes `CLA Assistant v3` fail and blocks the merge:

- a `Co-Authored-By:` trailer, for example
  `Co-Authored-By: Claude <noreply@anthropic.com>`. Do not add them in this
  repo; attribution in the pull request body is fine.
- a commit GitHub makes on the branch's behalf (`gh pr update-branch`, the
  "Update branch" button, a suggestion applied in the web UI), which is
  committed by `GitHub <noreply@github.com>`.

If one is already pushed, rewrite it locally (amend the message, or redo the
merge with `git merge origin/main`) and force-push the branch.

## When to stop, and what stopping means

Stop only when the next step needs something only Lawrence can supply:

- **His identity.** A browser OAuth login, a 2FA prompt, a Slack invite, an
  approval that must be made as him.
- **His judgment on the product.** Does this feel right, is this the UX we want,
  should this be the default. That is dogfooding, and it is the only kind of
  review worth his time.
- **His authority.** Spending money, deleting customer data, publishing
  something public, anything irreversible that affects other people.
- **His hardware.** A physical device, a cable, a machine that is not reachable.

Do not stop for anything else. Verification, CI runs, PR review, deployment,
cleanup, monitoring, infrastructure naming, or a choice between two reasonable
options are all the agent's job. When two options are defensible, pick the
better one, say which and why in one sentence, and keep going. A question that
the filesystem, the API, or a test could answer is not a question for him.
If a check, deployment, or background command is still queued or running,
monitor it to a completed success or failure instead of handing the wait to
him.

Never ask him to run a verification command. If a command proves the work, run
it and report the output. Never ask him to review a pull request; if the code
needs a second opinion, that is what review bots and tests are for.

When stopping is genuinely warranted, the handover is an invitation to use the
product, not a checklist:

- one command that exercises the thing, copy-pasteable, that works from his
  machine with no ssh, no admin token, and no environment setup
- what he should see, in one line
- the single question being asked, if there is one
- anything still unverified, named as such

"Review PR 118 and confirm CI is green" is homework. "Run `sr login`, you should
get a browser prompt and land back at a shell that says which team you are in"
is a handover.
