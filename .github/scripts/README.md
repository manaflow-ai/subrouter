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
