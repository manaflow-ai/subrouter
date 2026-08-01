# subrouter

Rust service for routing AI coding-agent traffic across subscription accounts and API keys.

## Development

- Use `cargo test --locked --all-targets --all-features` before handing off changes.
- Keep credential handling read-only unless a command explicitly delegates to the upstream account manager, such as `cx`.
- Do not log access tokens, refresh tokens, API keys, request bodies, or complete Authorization headers.
- Keep `rust-toolchain.toml`, `Cargo.toml`, and `Cargo.lock` aligned so local and CI builds use the same Rust and crate versions.

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
cargo test --locked --all-targets --all-features 2>&1 | head -5; echo SUITE_OK
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
cargo test --locked --all-targets --all-features > /tmp/subrouter-test.log 2>&1 && echo PASS || { echo FAIL; grep -E '(^failures:|test result: FAILED|panicked at)' /tmp/subrouter-test.log | head -20; }
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
