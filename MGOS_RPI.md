# MGOS appliance image for Raspberry Pi

`make mgos-rpi-image` produces a **bootable SD-card image** — the Go
counterpart of upstream STOS: flash it, power on, Movian comes up
fullscreen. Built on the last Raspberry Pi OS Lite **Buster** armhf
image (`2021-05-07`), the newest base that still ships the complete
`/opt/vc` legacy stack (dispmanx + OpenMAX IL + mmal) this port links
against.

```bash
make mgos-rpi-image
# -> build/mgos-rpi/movian-go-mgos-rpi-<version>.img.xz (+ .sha256)

xz -dc movian-go-mgos-rpi-*.img.xz | sudo dd of=/dev/sdX bs=4M status=progress conv=fsync
```

First boot expands the rootfs to fill the SD and reboots; then connman
brings up networking (Ethernet is plug&play — WiFi is configured from
Movian's network UI) and Movian autostarts via systemd with
`Restart=always`.

## What's inside the image

- `/opt/movian-go/` — binary (tags `rpi connman mgos webpopup libcec`)
  plus `glwskins`/`res`/`lang`
- **ConnMan** owns all interfaces; `dhcpcd` is disabled so nothing races
  it. `/etc/resolv.conf` → `/var/run/connman/resolv.conf` via a systemd
  drop-in
- **CEC**: `bcm2835_cec` + `vchiq` in `modules-load.d`,
  `dtparam=i2c_vc=on` — the pure-Go `/dev/cec*` backend picks up the
  adapter and the TV remote works
- **mgos layout**: `/mgos/{cache,persistent,fsinfo,media}`,
  `/mgosversion`, `/boot/dl` (update download dir used by
  `internal/upgrade/mgos.go`)
- **USB automount**: a udev rule + `mgos-media.sh` write/remove fsinfo
  files under `/mgos/fsinfo/<uuid>`; the app mounts/unmounts itself
  (upstream: `stos_automount`)
- `config.txt`: `gpu_mem=256`, `start_x=1` (extended firmware for
  mmal/OMX), vc4-kms/fkms overlays commented out
- hostname `movian`, machine-id regenerated per device on first boot
- `webpopup` is the **stub** — CEF ships no armv6 build

## Supported boards — not all Pis

| Board | Status |
|---|---|
| Pi 1 A/B/A+/B+, Pi Zero / Zero W / Zero 2 W | ✅ (see RAM note below) |
| Pi 2 B, Pi 3 B/B+ | ✅ |
| Pi 4 B, Pi 400, CM4 | ✅ — use **HDMI0** (port nearest USB-C) |
| **Pi 5 / CM5** | ❌ — Buster has no BCM2712 kernel and Pi 5 dropped the legacy dispmanx/OMX stack entirely |

## Per-board caveats

### Pi 1 / Pi Zero (256–512 MB, single-core armv6)

- **RAM is tight**: `gpu_mem=256` works on 512 MB boards; on **256 MB
  boards (Pi 1 A/A+, early Zero) it won't boot** — edit `config.txt` on
  the boot partition → `gpu_mem=128`
- OMX handles H.264 up to ~1080p30; the GLW UI at 60fps is modest on a
  single core — usable, not snappy
- **MPEG2/VC-1 (DVD!)** need the paid codec licenses in `config.txt` —
  without them playback falls back to software and a Pi 1 stutters on
  MPEG2. Pi 3/4 cope in software
- Pi Zero (non-W) has no onboard networking — USB ethernet/wifi dongle
  with an in-kernel driver required

### Pi 3 / 3B+

- The cleanest case: single HDMI, reliable CEC, onboard WiFi managed by
  connman. 1 GB RAM makes `gpu_mem=256` fine

### Pi 4 / 400 / CM4

- **HDMI0 only** — dispmanx drives a single output; HDMI1 stays black.
  CEC also lives on HDMI0
- **4K TVs**: the firmware accepts 4K modes but dispmanx compositing
  gets heavy — force 1080p if the UI feels sluggish:
  ```
  hdmi_group=1
  hdmi_mode=16    # 1080p60
  ```
- USB3 ports work for automount, but self-powered 2.5" HDDs can brown
  out — use powered drives/hubs

### All boards

- **Power**: undervoltage → intermittent OMX/USB errors. Use a proper
  supply, not a phone charger
- **Audio**: HDMI by default; the 3.5 mm jack is already enabled
  (`dtparam=audio=on`)
- **Overscan** on old TVs: add `disable_overscan=1` or the `overscan_*`
  lines to `config.txt` if you see black borders or cropped edges
- SD card: ≥4 GB, decent class — the UI is I/O-sensitive
- Serial console stays on `serial0` — handy for first-boot debugging

## Troubleshooting

If the box doesn't come up, plug a keyboard or a UART adapter:

```bash
journalctl -u movian-go -b     # app log
journalctl -u connman -b       # network manager
connmanctl technologies        # eth/wifi state
tvservice -s                   # current display mode
vcgencmd version               # firmware alive?
```

Logs and persistent state live under `/mgos/persistent/movian-go` and
`/mgos/cache/movian-go`.

## Updating the image itself

The in-app mgos updater reads `/mgosversion` and fetches artifacts into
`/boot/dl` (`internal/upgrade/mgos.go`) — it can self-update the
application payload. The OS base (kernel, connman) is updated only by
refashing a newly built image.
