# subrouter

Go service for routing AI coding-agent traffic across subscription accounts and API keys.

## Development

- Use `go test ./...` before handing off changes.
- Keep credential handling read-only unless a command explicitly delegates to the upstream account manager, such as `cx`.
- Do not log access tokens, refresh tokens, API keys, request bodies, or complete Authorization headers.
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

Merge only after the run completes and passes:

```
gh pr checks <PR> --repo manaflow-ai/subrouter --watch
gh pr merge <PR> --repo manaflow-ai/subrouter --squash --delete-branch
```

If CI is red on `main`, fixing it comes before any other work, including work
that was already in progress.

## Stopping means handing over exact commands

Never end a turn with a description of what could be done. End it with the
literal commands to run and the literal things to look at:

- the shell commands, copy-pasteable, in the order they should be run
- what each one should print when it worked
- which files or PR URLs to review, by path and number, not by description
- what is still not verified, named as such

"You should check the alert fired" is not a handover. "Run `gcloud logging read
...`, expect one line containing `[ALERT]`, and review PR 114" is.
