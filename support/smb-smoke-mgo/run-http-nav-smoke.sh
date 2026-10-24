#!/usr/bin/env bash
# Opt-in smoke against an ALREADY RUNNING Movian-Go with its own test profile.
# Requires local HTTP Control; does not inject credentials or install plugins.
set -euo pipefail
: "${SMB_SMOKE_HOST:?set SMB_SMOKE_HOST}"
: "${SMB_SMOKE_SHARE:?set SMB_SMOKE_SHARE}"
: "${SMB_SMOKE_FILE:?set SMB_SMOKE_FILE (fixture filename)}"
base="${MGO_SMOKE_HTTP:-http://127.0.0.1:42000}"
case "$base" in
  http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
  *) echo 'Refusing to control a non-loopback instance' >&2; exit 2 ;;
esac
: "${SMB_SMOKE_PORT:=445}"
host="$SMB_SMOKE_HOST"
if [[ "$SMB_SMOKE_PORT" != 445 ]]; then
  host="$host:$SMB_SMOKE_PORT"
fi
url="smb://$host/$SMB_SMOKE_SHARE"
if [[ -n "${SMB_SMOKE_PATH:-}" ]]; then
  path="${SMB_SMOKE_PATH#/}"; path="${path%/}"
  url="$url/$path"
fi
umask 077
art="${SMB_SMOKE_ART:-$(mktemp -d "${TMPDIR:-/tmp}/mgo-smb-nav-smoke.XXXXXXXX")}"
mkdir -p "$art"
prop() {
  curl --noproxy '*' -fsS --max-time 5 "$base/api/prop/$1" 2>/dev/null |
    sed -n '1s/^.* is a //p' | tr -d '\r' || true
}
open_url() {
  local target="${MGO_SMOKE_OPEN_PREFIX:-}$1"
  curl --noproxy '*' -fsS --max-time 5 --get --data-urlencode "url=$target" "$base/api/open" >/dev/null
}
wait_page() {
  local expected="$1"; local got loading
  for ((i=0;i<100;i++)); do
    got="$(prop 'global/navigators/current/currentpage/url')"
    loading="$(prop 'global/navigators/current/currentpage/model/loading')"
    if [[ "$got" == "$expected" || "$got" == "$expected/" || "$got" == "${MGO_SMOKE_OPEN_PREFIX:-}$expected" ]]; then
      if [[ "$loading" == 0 || "$loading" == '(void)' || -z "$loading" ]]; then
        return 0
      fi
    fi
    sleep 0.3
  done
  echo "Navigation timeout: want=$expected got=$got loading=$loading" >&2
  return 1
}
dump_nodes() {
  local case_name="$1"; local i title node_type node_url
  local case_file="$art/${case_name}.txt"
  : > "$case_file"
  echo "[$case_name] page=$(prop 'global/navigators/current/currentpage/url')" | tee -a "$case_file"
  for ((i=0;i<80;i++)); do
    title="$(prop "global/navigators/current/currentpage/model/nodes/*$i/metadata/title")"
    node_type="$(prop "global/navigators/current/currentpage/model/nodes/*$i/type")"
    node_url="$(prop "global/navigators/current/currentpage/model/nodes/*$i/url")"
    if [[ -n "$title$node_type$node_url" ]]; then
      printf '%s\t%s\t%s\n' "$title" "$node_type" "$node_url" | tee -a "$case_file"
    fi
  done
  cat "$case_file" >> "$art/summary.txt"
}
curl --noproxy '*' -fsS --max-time 5 "$base/api/diag" >/dev/null || {
  echo "MGO HTTP Control not reachable at $base" >&2; exit 2;
}
if [[ "${SMB_SMOKE_SKIP_HOST_ENUM:-0}" != 1 ]]; then
  index=0
  for target in "smb://$host" "smb://$host/"; do
    open_url "$target"; wait_page "$target"; dump_nodes "host-$index"
    grep -Fqi "$SMB_SMOKE_SHARE" "$art/host-$index.txt" || {
      echo "Host listing missing expected share $SMB_SMOKE_SHARE" >&2; exit 1;
    }
    index=$((index+1))
  done
fi
open_url "$url/"; wait_page "$url/"; dump_nodes share-dir
if ! grep -Fqi "$SMB_SMOKE_FILE" "$art/share-dir.txt"; then
  echo "Directory listing missing fixture $SMB_SMOKE_FILE" >&2; exit 1
fi
printf 'Navigation smoke passed. Artifacts: %s\n' "$art"
