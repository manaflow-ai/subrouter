#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$HERE/../subrouter-deploy.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/bin" "$ROOT/state" "$ROOT/etc" "$ROOT/revisions" "$ROOT/fakebin"
git init --bare -q "$ROOT/repo.git"
git init -q "$ROOT/work"
git -C "$ROOT/work" config user.email test@example.invalid
git -C "$ROOT/work" config user.name test
printf base >"$ROOT/work/base"
git -C "$ROOT/work" add base
git -C "$ROOT/work" commit -qm base
base="$(git -C "$ROOT/work" rev-parse HEAD)"
git -C "$ROOT/work" push -q "$ROOT/repo.git" HEAD:main
printf candidate >"$ROOT/work/candidate"
git -C "$ROOT/work" add candidate
git -C "$ROOT/work" commit -qm candidate
candidate="$(git -C "$ROOT/work" rev-parse HEAD)"
git -C "$ROOT/work" push -q "$ROOT/repo.git" HEAD:main
git -C "$ROOT/work" checkout -qb unrelated "$base"
printf unrelated >"$ROOT/work/unrelated"
git -C "$ROOT/work" add unrelated
git -C "$ROOT/work" commit -qm unrelated
unrelated="$(git -C "$ROOT/work" rev-parse HEAD)"
git -C "$ROOT/work" push -q "$ROOT/repo.git" HEAD:unrelated
git -C "$ROOT/work" checkout -q -

printf '#!/bin/sh\n# incumbent\nexit 0\n' >"$ROOT/bin/subrouter"
printf '#!/bin/sh\n# candidate\nexit 0\n' >"$ROOT/candidate"
printf '#!/bin/sh\n# unrelated\nexit 0\n' >"$ROOT/unrelated"
chmod 0755 "$ROOT/bin/subrouter" "$ROOT/candidate" "$ROOT/unrelated"
cat >"$ROOT/fakebin/go" <<'FAKEGO'
#!/bin/sh
file="$3"
case "$file" in
  *unrelated) revision="$UNRELATED_REV" ;;
  *candidate) revision="$CANDIDATE_REV" ;;
  *) revision="$BASE_REV" ;;
esac
printf 'path\texample.test/subrouter\nbuild\tvcs.revision=%s\nbuild\tvcs.modified=false\n' "$revision"
FAKEGO
chmod 0755 "$ROOT/fakebin/go"
export BASE_REV="$base" CANDIDATE_REV="$candidate" UNRELATED_REV="$unrelated"
export PATH="$ROOT/fakebin:$PATH"
export SUBROUTER_BIN="$ROOT/bin/subrouter"
export SUBROUTER_DEPLOY_STATE="$ROOT/state"
export SUBROUTER_LAST_GOOD="$ROOT/state/last-good"
export SUBROUTER_VERSION_FILE="$ROOT/etc/version"
export SUBROUTER_HEALTH_URL="file://$ROOT/health"
export SUBROUTER_CONTROL_SOCKET="$ROOT/missing.sock"
export SUBROUTER_DEPLOY_REPO_URL="$ROOT/repo.git"
export SUBROUTER_DEPLOY_REPO_CACHE="$ROOT/cache.git"
export SUBROUTER_DEPLOY_REVISIONS_DIR="$ROOT/revisions"
printf ok >"$ROOT/health"
printf '%s\n' "$base" >"$ROOT/revisions/$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"

if bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1; then
  echo 'FAIL missing revision accepted'; exit 1
fi
if bash "$DEPLOY" install "$ROOT/candidate" --allow-unrelated bootstrap >/dev/null 2>&1; then
  echo 'FAIL allow-unrelated bypass accepted'; exit 1
fi
if bash "$DEPLOY" install "$ROOT/unrelated" --revision "$unrelated" >/dev/null 2>&1; then
  echo 'FAIL unrelated main revision accepted'; exit 1
fi
if bash "$DEPLOY" install "$ROOT/candidate" --revision "$candidate" >/dev/null 2>&1; then
  echo 'FAIL candidate unexpectedly installed without supervisor'; exit 1
fi
rm -f "$ROOT/revisions/$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
if bash "$DEPLOY" install "$ROOT/candidate" --revision "$candidate" >/dev/null 2>&1; then
  echo 'FAIL missing live revision accepted'; exit 1
fi
echo 'deploy lineage guard: PASS'
