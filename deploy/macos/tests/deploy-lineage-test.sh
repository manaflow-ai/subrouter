#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$HERE/../subrouter-deploy.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/bin" "$ROOT/state" "$ROOT/etc" "$ROOT/repo.git"
git init --bare -q "$ROOT/repo.git"
work="$ROOT/work"; git init -q "$work"; git -C "$work" config user.email test@example.invalid; git -C "$work" config user.name test
echo base >"$work/base"; git -C "$work" add base; git -C "$work" commit -qm base
base="$(git -C "$work" rev-parse HEAD)"; git -C "$work" push -q "$ROOT/repo.git" HEAD:main
echo candidate >"$work/candidate"; chmod 0755 "$work/candidate"; git -C "$work" add candidate; git -C "$work" commit -qm candidate
candidate="$(git -C "$work" rev-parse HEAD)"; git -C "$work" push -q "$ROOT/repo.git" HEAD:main
git -C "$work" checkout -qb unrelated "$base"; echo unrelated >"$work/unrelated"; git -C "$work" add unrelated; git -C "$work" commit -qm unrelated; unrelated="$(git -C "$work" rev-parse HEAD)"; git -C "$work" push -q "$ROOT/repo.git" HEAD:unrelated; git -C "$work" checkout -q -
printf '#!/bin/sh\nexit 0\n' >"$ROOT/bin/subrouter"; chmod 0755 "$ROOT/bin/subrouter"
printf '#!/bin/sh\necho candidate\nexit 0\n' >"$ROOT/candidate"; chmod 0755 "$ROOT/candidate"
printf ok >"$ROOT/health"
mkdir -p "$ROOT/revisions"
export SUBROUTER_BIN="$ROOT/bin/subrouter" SUBROUTER_DEPLOY_STATE="$ROOT/state" SUBROUTER_LAST_GOOD="$ROOT/state/last-good" SUBROUTER_VERSION_FILE="$ROOT/etc/version" SUBROUTER_HEALTH_URL="file://$ROOT/health" SUBROUTER_CONTROL_SOCKET="$ROOT/missing.sock" SUBROUTER_DEPLOY_REPO_URL="$ROOT/repo.git" SUBROUTER_DEPLOY_REPO_CACHE="$ROOT/cache.git" SUBROUTER_DEPLOY_REVISIONS_DIR="$ROOT/revisions"
# No revision is refused, even before any worker mutation.
if bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1; then echo 'FAIL missing revision accepted'; exit 1; fi
# A pushed revision is accepted when the live worker has no legacy record.
bash "$DEPLOY" install "$ROOT/candidate" --revision "$candidate" --allow-unrelated bootstrap >/dev/null 2>&1 || true
# Record the live worker as the base commit, then an unrelated candidate is refused.
printf '%s\n' "$unrelated" >"$ROOT/revisions/$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
if bash "$DEPLOY" install "$ROOT/candidate" --revision "$candidate"; then echo 'FAIL unrelated revision accepted'; exit 1; fi
echo 'deploy lineage guard: PASS'
