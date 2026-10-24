#!/usr/bin/env bash
# Run from the root of a Movian-Go checkout, after copying the Go test file.
set -euo pipefail
: "${SMB_SMOKE_HOST:?set SMB_SMOKE_HOST}"
: "${SMB_SMOKE_SHARE:?set SMB_SMOKE_SHARE}"
: "${SMB_SMOKE_FILE:?set SMB_SMOKE_FILE (fixture filename)}"
: "${SMB_SMOKE_SHA256:?set SMB_SMOKE_SHA256 (fixture digest)}"
command -v go >/dev/null || { echo 'Go toolchain not found' >&2; exit 2; }
export SMB_SMOKE_ENABLE=1
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
umask 077
art="${SMB_SMOKE_ART:-$(mktemp -d "${TMPDIR:-/tmp}/mgo-smb-client-smoke.XXXXXXXX")}"
mkdir -p "$art"
log="$art/go-test.log"
cleanup() {
  if [[ -n "${SMB_SMOKE_PASSWORD:-}" && -f "$log" ]]; then
    # Never leave credentials in the archived text logs. The secret goes
    # through the environment, not argv, so `ps` cannot show it.
    SMB_REDACT_SECRET="$SMB_SMOKE_PASSWORD" python3 - "$log" <<'PY'
import os, pathlib, sys
p = pathlib.Path(sys.argv[1]); secret = os.environ['SMB_REDACT_SECRET']
p.write_text(p.read_text(errors='replace').replace(secret, '<redacted>'))
PY
  fi
}
trap cleanup EXIT
printf 'Target: %s:%s, share: %s, fixture: %s\n' "$SMB_SMOKE_HOST" "${SMB_SMOKE_PORT:-445}" "$SMB_SMOKE_SHARE" "$SMB_SMOKE_FILE"
set +e
go test -count=1 -v -timeout 8m ./internal/fileaccess/smb -run '^TestSMB2RemoteSmoke$' > "$log" 2>&1
rc=$?
set -e
cleanup
cat "$log"
printf 'Result=%s Artifact=%s\n' "$rc" "$art"
exit "$rc"
