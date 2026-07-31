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
