#!/usr/bin/env bash
# Create a local disposable file to copy into your isolated SMB test share.
# Does NOT copy/write anything to a server. Fails if file already exists.
set -euo pipefail
if [[ $# != 1 ]]; then echo "Usage: $0 /path/to/local/test-fixture.bin" >&2; exit 2; fi
file="$1"
if [[ -e "$file" ]]; then echo 'Refusing to overwrite existing file' >&2; exit 2; fi
python3 - "$file" <<'PY'
import hashlib, pathlib, sys
p=pathlib.Path(sys.argv[1]); p.parent.mkdir(parents=True, exist_ok=True)
# Deterministic 1 MiB fixture, with differences throughout for seek checks.
seed=hashlib.sha256(b'Movian Go SMB2/3 smoke fixture v1').digest()
with p.open('xb') as f:
    for i in range(16384):
        f.write(hashlib.sha256(seed + i.to_bytes(8,'little')).digest()*2)
print('SMB_SMOKE_FILE=' + p.name)
print('SMB_SMOKE_SHA256=' + hashlib.sha256(p.read_bytes()).hexdigest())
print('Copy this local file into your isolated SMB_SMOKE_PATH directory before testing.')
PY
