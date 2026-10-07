#!/usr/bin/env bash
# make_mgos_rpi_image.sh — build a bootable mgos (appliance) SD image for
# Raspberry Pi armv6 (Pi 1/Zero/2/3/4 — 32-bit userland only).
#
# Base: Raspberry Pi OS Lite Buster (2021-05-07) — the last release shipping
# the complete /opt/vc legacy stack (dispmanx + OMX + mmal) the `rpi` port
# links against. Bullseye removed it; do not upgrade the base image.
#
# The script runs fully unprivileged: FAT boot partition via mtools, ext4
# root partition via dd split + debugfs, Debian packages via dpkg-deb -x.
#
# Requirements: curl xz unzip fdisk debugfs mtools dpkg-deb awk sed sort comm
#
# Usage:
#   ./scripts/make_mgos_rpi_image.sh [--image-url URL] [--out NAME]
# Output: build/mgos-rpi/movian-go-mgos-rpi-<ver>.img.xz (+ .sha256)

set -euo pipefail
PATH="/usr/sbin:/sbin:$PATH"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

IMG_URL="https://downloads.raspberrypi.com/raspios_lite_armhf/images/raspios_lite_armhf-2021-05-28/2021-05-07-raspios-buster-armhf-lite.zip"
RASPBIAN="http://legacy.raspbian.org/raspbian"
PKG_INDEX="$RASPBIAN/dists/buster/main/binary-armhf/Packages.xz"
RPI_TAGS="rpi connman mgos webpopup libcec"

WORK="$REPO_ROOT/build/mgos-rpi"
VERSION="$(git describe --tags --dirty --abbrev=5 2>/dev/null || echo 0.0.0)"
VERSION="${VERSION%-dirty}"

while [ $# -gt 0 ]; do
	case "$1" in
	--image-url) IMG_URL="$2"; shift 2 ;;
	--out) WORK="$2"; shift 2 ;;
	*) echo "unknown option: $1" >&2; exit 1 ;;
	esac
done

log() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "missing tool: $1"; }

for t in curl xz unzip fdisk debugfs mtype mcopy mmd dpkg-deb awk sed comm sort; do
	need "$t"
done

mkdir -p "$WORK"

# --------------------------------------------------------------------------
# 1. Build the appliance binary (same tags as the release RPi job)
# --------------------------------------------------------------------------
BIN="$REPO_ROOT/movian-go-rpi"
if [ ! -x "$BIN" ]; then
	log "Building movian-go-rpi (tags: $RPI_TAGS)"
	make rpi RPI_TAGS="$RPI_TAGS"
fi
arm-linux-gnueabihf-strip -o "$BIN.stripped" "$BIN" 2>/dev/null || cp "$BIN" "$BIN.stripped"

# --------------------------------------------------------------------------
# 2. Fetch + unpack the base image (cached in WORK)
# --------------------------------------------------------------------------
IMG_ZIP="$WORK/$(basename "$IMG_URL")"
IMG_XZ_FALLBACK="$IMG_ZIP"
if [ ! -f "$IMG_ZIP" ]; then
	log "Downloading base image"
	curl -fL -o "$IMG_ZIP" "$IMG_URL"
fi
case "$IMG_ZIP" in
*.zip) unzip -o -q "$IMG_ZIP" -d "$WORK" ;;
*.img.xz) xz -dk "$IMG_ZIP" ;;
esac
BASE_IMG="$(find "$WORK" -maxdepth 1 -name '*.img' | head -1)"
[ -n "$BASE_IMG" ] || die "no .img found after unpacking $IMG_ZIP"
OUT_IMG="$WORK/movian-go-mgos-rpi-$VERSION.img"
[ -f "$OUT_IMG" ] || cp "$BASE_IMG" "$OUT_IMG"
IMG="$OUT_IMG"
log "Base image: $(basename "$BASE_IMG") -> $(basename "$IMG")"

# --------------------------------------------------------------------------
# 3. Partition offsets (bytes)
# --------------------------------------------------------------------------
part_info() { # img partnum -> "start sectors" in 512B units
	fdisk -l -o Device,Start,Sectors "$1" 2>/dev/null |
		awk -v n="$2" '$1 ~ (n "$") {print $2, $3; exit}'
}
read -r P1_START _ <<<"$(part_info "$IMG" 1)"
read -r P2_START P2_SECTORS <<<"$(part_info "$IMG" 2)"
[ -n "$P1_START" ] && [ -n "$P2_START" ] || die "cannot parse partition table"
BOOT_OFF=$((P1_START * 512))
log "Partitions: boot @ sector $P1_START (offset $BOOT_OFF), rootfs @ sector $P2_START ($P2_SECTORS sectors)"

# --------------------------------------------------------------------------
# 4. Boot partition (FAT) edits via mtools
# --------------------------------------------------------------------------
log "Editing boot partition"
TMPD="$(mktemp -d)"
trap 'rm -rf "$TMPD"' EXIT

mtype -i "$IMG@@$BOOT_OFF" ::config.txt >"$TMPD/config.txt" ||
	die "cannot read config.txt from boot partition"
# Legacy dispmanx/OMX needs vc4-kms/fkms OFF and enough GPU memory.
sed -i 's/^dtoverlay=vc4-\(k\|fk\)ms-v3d/#dtoverlay=vc4-\1ms-v3d/' "$TMPD/config.txt"
grep -q '^gpu_mem=' "$TMPD/config.txt" &&
	sed -i 's/^gpu_mem=.*/gpu_mem=256/' "$TMPD/config.txt" ||
	echo "gpu_mem=256" >>"$TMPD/config.txt"
# start_x loads the extended firmware (mmal/OpenMAX support)
grep -q '^start_x=' "$TMPD/config.txt" &&
	sed -i 's/^start_x=.*/start_x=1/' "$TMPD/config.txt" ||
	echo "start_x=1" >>"$TMPD/config.txt"
# CEC userspace driver (bcm2835_cec talks through the firmware)
grep -q '^dtparam=i2c_vc=on' "$TMPD/config.txt" ||
	echo "dtparam=i2c_vc=on" >>"$TMPD/config.txt"
mcopy -o -i "$IMG@@$BOOT_OFF" "$TMPD/config.txt" ::config.txt
mmd -i "$IMG@@$BOOT_OFF" ::dl 2>/dev/null || true # /boot/dl — mgos update download dir

# --------------------------------------------------------------------------
# 5. Split out the ext4 rootfs so debugfs can write it without offsets
# --------------------------------------------------------------------------
ROOTFS="$WORK/rootfs.img"
[ -f "$ROOTFS" ] || {
	log "Extracting rootfs partition for surgery"
	dd if="$IMG" of="$ROOTFS" bs=4M "iflag=skip_bytes,count_bytes" \
		skip=$((P2_START * 512)) count=$((P2_SECTORS * 512)) status=none
}

# debugfs helpers — every call pipes a command list; mkdir/sif failures on
# already-existing entries are expected, so the helpers stay lenient.
dbg() { debugfs -w -f - "$ROOTFS" >/dev/null 2>&1 || true; }
dbg_cat() { debugfs -R "cat $1" "$ROOTFS" 2>/dev/null; }
dbg_mkdir_p() {
	local p="" c
	for c in $(echo "${1#/}" | tr '/' ' '); do
		p="$p/$c"; echo "mkdir $p"
	done | dbg
}
dbg_put() { # src dst [mode] — mode like 755/644 (regular-file bits implied)
	local src="$1" dst="$2" mode="${3:-644}"
	dbg_mkdir_p "$(dirname "$dst")"
	printf 'rm %s\nwrite %s %s\n' "$dst" "$src" "$dst" | dbg
	printf 'sif %s mode 0100%s\n' "$dst" "$mode" | dbg
}
# dbg_symlink target link — debugfs symlink cmd on modern e2fsprogs
dbg_symlink() { printf 'rm %s\nsymlink %s %s\n' "$2" "$2" "$1" | dbg; }
dbg_rm() { printf 'rm %s\n' "$1" | dbg; }

# Install a local tree (files + dirs + symlinks) into the image.
# dbg_install_tree <localdir> <imgdir>
dbg_install_tree() {
	local root="$1" dst="$2" f rel mode tgt
	dbg_mkdir_p "$dst"
	find "$root" -mindepth 1 | while read -r f; do
		rel="${f#$root}"
		if [ -d "$f" ]; then
			dbg_mkdir_p "$dst$rel"
		elif [ -L "$f" ]; then
			tgt="$(readlink "$f")"
			case "$tgt" in /*) dbg_symlink "$tgt" "$dst$rel" ;;
			*) dbg_symlink "$(dirname "$dst$rel")/$tgt" "$dst$rel" ;;
			esac
		elif [ -f "$f" ]; then
			mode=644
			[ -x "$f" ] && mode=755
			dbg_put "$f" "$dst$rel" "$mode"
		fi
	done
}

# --------------------------------------------------------------------------
# 6. Rootfs surgery
# --------------------------------------------------------------------------
log "Installing movian-go appliance into rootfs"
dbg_mkdir_p /opt/movian-go
dbg_put "$BIN.stripped" /opt/movian-go/movian-go 755
for d in glwskins res lang; do
	[ -d "$REPO_ROOT/$d" ] && dbg_install_tree "$REPO_ROOT/$d" "/opt/movian-go/$d"
done
rm -f "$BIN.stripped"

# mgos filesystem layout (C: support/stos paths — paths.go)
dbg_mkdir_p /mgos/cache/movian-go
dbg_mkdir_p /mgos/persistent/movian-go
dbg_mkdir_p /mgos/fsinfo
dbg_mkdir_p /mgos/media
echo "$VERSION" >"$TMPD/mgosversion"
dbg_put "$TMPD/mgosversion" /mgosversion

# USB media -> /mgos/fsinfo/<uuid> notify files; the app mounts itself
cat >"$TMPD/mgos-media.sh" <<'EOF'
#!/bin/sh
# Called by udev for block devices; writes/removes /mgos/fsinfo/<uuid>
# entries that movian-go's automount watcher consumes.
FSINFO=/mgos/fsinfo
mkdir -p "$FSINFO"
case "$1" in
add)
	[ -n "${ID_FS_UUID:-}" ] || exit 0
	printf 'DEVNAME=/dev/%s\nID_FS_UUID=%s\nID_FS_TYPE=%s\nID_FS_LABEL=%s\n' \
		"$2" "$ID_FS_UUID" "${ID_FS_TYPE:-}" "${ID_FS_LABEL:-}" >"$FSINFO/$ID_FS_UUID"
	;;
remove)
	for f in "$FSINFO"/*; do
		[ -f "$f" ] || continue
		grep -q "DEVNAME=/dev/$2\$" "$f" && rm -f "$f"
	done
	;;
esac
EOF
dbg_put "$TMPD/mgos-media.sh" /opt/movian-go/mgos-media.sh 755
cat >"$TMPD/99-mgos-media.rules" <<'EOF'
ACTION=="add|change", SUBSYSTEM=="block", ENV{DEVTYPE}=="partition", ENV{ID_FS_UUID}=="?*", RUN+="/opt/movian-go/mgos-media.sh add %k"
ACTION=="remove", SUBSYSTEM=="block", ENV{DEVTYPE}=="partition", RUN+="/opt/movian-go/mgos-media.sh remove %k"
EOF
dbg_put "$TMPD/99-mgos-media.rules" /etc/udev/rules.d/99-mgos-media.rules

# Kernel modules: vchiq (VideoCore IPC — OMX/dispmanx) and bcm2835_cec
# (kernel CEC adapter used by the pure-Go libcec backend).
printf 'vchiq\nbcm2835_cec\n' >"$TMPD/mgos-modules.conf"
dbg_put "$TMPD/mgos-modules.conf" /etc/modules-load.d/mgos.conf

# --------------------------------------------------------------------------
# 7. Connman (+ missing deps) injected from the raspbian buster archive
# --------------------------------------------------------------------------
log "Installing connman into rootfs"
DBG_STATUS="$TMPD/status"
dbg_cat /var/lib/dpkg/status >"$DBG_STATUS" || die "cannot read dpkg status"
grep '^Package: ' "$DBG_STATUS" | awk '{print $2}' | sort -u >"$TMPD/installed"
log "  $(wc -l <"$TMPD/installed") packages already in image"

PKGIDX="$WORK/Packages.xz"
[ -f "$PKGIDX" ] || curl -fsL -o "$PKGIDX" "$PKG_INDEX"

# Minimal index parse: package -> filename + dep-name list
parse_index() { # prints "name filename dep1,dep2,..."
	xzcat "$PKGIDX" | awk '
	/^Package: /  {if (pkg!="") printf "%s %s %s\n", pkg, fn, deps
	               pkg=$2; fn=""; deps=""}
	/^Filename: / {fn=$2}
	/^Depends: /  {sub(/^Depends: /,""); deps=$0}
	END {if (pkg!="") printf "%s %s %s\n", pkg, fn, deps}'
}
parse_index >"$TMPD/pkgidx"

fetch_deb() { # pkgname -> extracts deb into $TMPD/debstage
	local pkg="$1" line fn
	line=$(grep "^$pkg " "$TMPD/pkgidx") || die "package $pkg not in buster index"
	fn=$(echo "$line" | awk '{print $2}')
	[ -f "$WORK/$(basename "$fn")" ] || curl -fsL -o "$WORK/$(basename "$fn")" "$RASPBIAN/$fn"
	mkdir -p "$TMPD/debstage/$pkg"
	dpkg-deb -x "$WORK/$(basename "$fn")" "$TMPD/debstage/$pkg"
	log "  installed deb: $pkg ($(basename "$fn"))"
}

fetch_deps() { # pkg -> fetch pkg + recursively missing deps
	local pkg="$1" d
	fetch_deb "$pkg"
	local deps
	deps=$(grep "^$pkg " "$TMPD/pkgidx" | cut -d' ' -f3- | tr ',' '\n' |
		sed 's/ *([^)]*)//g; s/|.*//; s/:any//; s/^ *//; s/ *$//' | grep -v '^$')
	for d in $deps; do
		grep -qx "$d" "$TMPD/installed" && continue
		grep -qx "$d" "$TMPD/fetched" 2>/dev/null && continue
		echo "$d" >>"$TMPD/fetched"
		fetch_deps "$d"
	done
}
touch "$TMPD/fetched"
fetch_deps connman

# Inject all deb payload trees
for pkgdir in "$TMPD"/debstage/*/; do
	dbg_install_tree "$pkgdir" /
done

# --------------------------------------------------------------------------
# 8. systemd plumbing (regular files in .wants dirs — no symlinks needed)
# --------------------------------------------------------------------------
log "Configuring services"
cat >"$TMPD/movian-go.service" <<'EOF'
[Unit]
Description=Movian-Go media center (mgos appliance)
After=local-fs.target connman.service dbus.service
Wants=connman.service

[Service]
Type=simple
WorkingDirectory=/opt/movian-go
ExecStart=/opt/movian-go/movian-go
Restart=always
RestartSec=2
StandardInput=null
StandardOutput=journal
StandardError=journal
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
EOF
dbg_put "$TMPD/movian-go.service" /etc/systemd/system/movian-go.service
dbg_put "$TMPD/movian-go.service" /etc/systemd/system/multi-user.target.wants/movian-go.service

# Enable connman: copy its own unit into multi-user.target.wants
CONNSVC="$(find "$TMPD/debstage/connman" -name connman.service | head -1)"
[ -n "$CONNSVC" ] && dbg_put "$CONNSVC" /etc/systemd/system/multi-user.target.wants/connman.service

# connman writes /var/run/connman/resolv.conf — point /etc/resolv.conf at it
# via a service drop-in (avoids needing symlink creation inside the image)
cat >"$TMPD/resolvconf.conf" <<'EOF'
[Service]
ExecStartPre=/bin/ln -sf /var/run/connman/resolv.conf /etc/resolv.conf
EOF
dbg_put "$TMPD/resolvconf.conf" /etc/systemd/system/connman.service.d/resolvconf.conf

# dhcpcd would fight connman for the interfaces — disable it
dbg_rm /etc/systemd/system/multi-user.target.wants/dhcpcd.service
dbg_rm /etc/systemd/system/dhcpcd.service.d/wait.conf

# connman default config (prefer wired over wifi)
printf '[General]\nPreferredTechnologies=ethernet,wifi\n' >"$TMPD/main.conf"
dbg_put "$TMPD/main.conf" /etc/connman/main.conf

# Appliance identity
printf 'movian\n' >"$TMPD/hostname"
dbg_put "$TMPD/hostname" /etc/hostname
dbg_cat /etc/hosts | sed 's/raspberrypi/movian/g' >"$TMPD/hosts"
[ -s "$TMPD/hosts" ] && dbg_put "$TMPD/hosts" /etc/hosts
: >"$TMPD/machine-id" # empty -> systemd regenerates per-device on first boot
dbg_put "$TMPD/machine-id" /etc/machine-id
dbg_mkdir_p /var/lib/connman # connman state dir (postinst would create it)

# --------------------------------------------------------------------------
# 9. Merge rootfs back, compress, checksum
# --------------------------------------------------------------------------
log "Merging rootfs back into image"
dd if="$ROOTFS" of="$IMG" bs=4M "oflag=seek_bytes" \
	seek=$((P2_START * 512)) conv=notrunc status=none
rm -f "$ROOTFS"

log "Compressing"
xz -f -9 "$IMG"
sha256sum "$IMG.xz" >"$IMG.xz.sha256"

log "Done: $IMG.xz"
log "Flash with: xz -dc $(basename "$IMG.xz") | sudo dd of=/dev/sdX bs=4M status=progress conv=fsync"
