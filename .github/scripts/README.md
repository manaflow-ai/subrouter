# CLA recheck authorizer

`cla-recheck-auth.sh` is the read-only admission step for an exact `recheck`
comment. The caller must provide the event comment ID, body, author ID, login,
and creation timestamp through environment variables. The caller needs
`issues:read` and `pull-requests:read`. The collaborator permission endpoint
also needs repository Metadata read. GitHub Actions supplies that metadata
permission implicitly for `GITHUB_TOKEN`; a caller that supplies a different
token must grant Metadata read explicitly. No Administration or write
permission is needed.

The script re-reads the issue, live Pull Request, comment, and collaborator
permission through bounded GitHub API requests. It accepts only an open,
unmerged Pull Request targeting `main`, an unchanged human comment, and a live
`admin` or `maintain` role. It never writes a comment, check, ledger, or source
file.

Each request is retried at most three times. A validated 404 or a changed
comment is `unauthorized` with `check_action=preserve`. A temporary API,
transport, or rate-limit failure is `retry` with `check_action=preserve`; the
caller must leave any existing required check untouched and run again later.
Malformed or incomplete responses are `error` with `check_action=fail`.

The workflow must execute this file from a trusted default-branch revision. It
must not check out or execute Pull Request head content in a
`pull_request_target` job.

The workflow's only required status context is the native job `CLA Assistant
v3`. The job name is produced by GitHub Actions, and the rerun helper binds the
workflow path, workflow ID, run, job, app ID 15368, source head, target base,
generation `v2.2-action-212a0f2dd659b24b48a30ba35966e06dc41736af`, and canonical
details URL before requesting a rerun. No Checks API check is created by a
comment event.

Signatures are written only by the maintained action at immutable SHA
`212a0f2dd659b24b48a30ba35966e06dc41736af` to
`cla-signatures:signatures/version2/cla.json`. That branch must remain protected
against deletion, force-push, and non-linear history, with only the GitHub
Actions writer bypass. The empty version 2 file is bootstrapped by an
administrator before the `CLA Assistant v3` context is made required on `main`.

`lock-merged-pr.sh` is the separate post-merge lock lane. It accepts only a
canonical `pull_request_target` close event, rechecks the live merged Pull
Request, grants no ledger or Actions write permission, and verifies the lock
after the PUT. Its API capture is bounded before JSON parsing and it does not
print response bodies.
