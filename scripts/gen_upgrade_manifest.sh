#!/bin/sh
# gen_upgrade_manifest.sh — emit OTA upgrade manifests consumed by
# internal/upgrade (checkUpgrade fetches <ctrlbase>/<track>-<arch>.json).
#
# The files are meant to be attached to the GitHub release itself: the
# upgrade.movian-go.czz78.com vhost 302-redirects /upgrade/3/<file> to
# releases/latest/download/<file>, so "latest" always serves the newest
# manifests with no server-side publishing step.
#
# Usage:
#   gen_upgrade_manifest.sh <version> <artifact-file> <artifact-type> <out-dir> <arch>...
#
# Example:
#   gen_upgrade_manifest.sh 8.4.0 dist/movian-go-rpi-update.bin sqfs dist rpi arm
#
# artifact-type must match what the client looks for: mgos builds
# select type "sqfs" (see internal/upgrade mgosStartUpgrade).
set -eu

VER="$1"; FILE="$2"; TYPE="$3"; OUT="$4"; shift 4
[ $# -ge 1 ] || { echo "usage: $0 <ver> <file> <type> <outdir> <arch>..." >&2; exit 1; }

SHA1=$(sha1sum "$FILE" | cut -d' ' -f1)
SIZE=$(stat -c%s "$FILE")
BASE=$(basename "$FILE")
URL="https://github.com/czz/movian-go/releases/download/$VER/$BASE"

mkdir -p "$OUT"
for ARCH in "$@"; do
    for TRACK in stable testing master; do
        cat > "$OUT/$TRACK-$ARCH.json" <<EOF
{
  "version": "$VER",
  "artifacts": [
    {
      "type": "$TYPE",
      "url": "$URL",
      "sha1": "$SHA1",
      "size": $SIZE,
      "name": "$BASE"
    }
  ],
  "changelog": [
    { "version": "$VER",
      "desc": "See https://github.com/czz/movian-go/releases/tag/$VER" }
  ]
}
EOF
    done
done

echo "generated $# × 3 manifests in $OUT (version $VER, sha1 $SHA1, $SIZE bytes)"
