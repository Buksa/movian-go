# Live TV

Movian has an integrated TV frontend — no Tvheadend required. It uses
the platform's native tuner stack:

- **Linux / MGOS** — DVB adapters (`/dev/dvb/adapterN`) tuned directly;
  the MPEG-TS stream goes through Movian's own media pipeline
  (`dvbts://`), so media info, recording hooks and subtitles behave
  like any other source.
- **Android TV** — the TV Input Framework (TIF). Movian hosts a
  `TvView`; the vendor's `TvInputService` performs tuning, demuxing,
  decoding and rendering behind Movian's GLW overlay.

A single "Live TV" service appears on the home screen on both
platforms.

## How it works (Android)

The video you see is **not** decoded by Movian's player. The vendor
`TvInputService` renders straight onto a `TvView` surface that sits
under the GLW UI. This is the only way on certified boxes — the TS is
usually not reachable by apps (e.g. on the TIM box the feeder files
under `/mnt/dtvfs` are permission-locked even to adb shell).

One `TvView` session is shared between two pages:

- **Channel list** (`tvinput:`) — channel list on the left, a live
  preview (PiP) top-right, EPG panel below it. Pausing on a channel
  for ~0.7 s zaps the preview to it.
- **Full screen** (`tvinput:play/<input>/<channel>`) — the playing
  channel plus OSD: channel/EPG sidebar, audio and subtitle track
  pickers, timeshift if the input supports it.

Moving between the two pages hands the session over without
re-tuning. Backing out of Live TV entirely untunes and releases the
session.

### Navigation

| Remote key | Action |
|---|---|
| Up / Down | Move in the channel list |
| Right | Enter the EPG panel |
| Left | Back to the channel list |
| OK / Enter | Channel list → tune preview; on an EPG row → tune that channel; in the list on a channel → full screen |
| Back | OSD closes first; next Back leaves the page (session released on exit) |
| PageUp / PageDown | Channel up/down during playback |

## Channel discovery (Android)

`Tv.listChannels()` merges two sources:

1. **`TvContract.Channels`** — the standard TIF provider. Populated
   automatically on normal Android TVs once you've done a channel
   scan in the device's own tuner app (Live Channels, vendor TV app).
   Reading rows written by *another* package requires the privileged
   `ACCESS_ALL_EPG_DATA` permission, which normal apps don't get —
   that's why on operator boxes the provider often reads back empty
   even though the box can tune channels.
2. **Sidecar** — when the provider returns zero channels,
   `<externalFilesDir>/tv_channels.json` is merged in. On the TIM box:
   `/sdcard/Android/data/com.moviango.mediaplayer/files/tv_channels.json`.

Inputs are enumerated at runtime from `TvInputManager` — nothing is
hardcoded per vendor. Vendor feeder URIs live in the sidecar as data,
not in the code.

### Sidecar format

```json
{
  "channels": [
    {
      "id": 109863,
      "inputId": "com.technicolor.android.dtvinput/.DtvInputService",
      "name": "Rete4 HD",
      "number": "104",
      "uri": "/mnt/dtvfs/live/dvb:110.3a2.bbc"
    }
  ],
  "epgUrl": "https://epgshare01.online/epgshare01/epg_ripper_IT1.xml.gz"
}
```

| Field | Meaning |
|---|---|
| `id` | `TvContract` rowid when the row exists; otherwise arbitrary/0 — the feeder URI is what matters |
| `inputId` | Component name of the `TvInputService` (`pkg/.Service` or `pkg/pkg.Service`) |
| `name` | Display name; also used for XMLTV channel matching |
| `number` | LCN — used for sorting and shown as the list prefix. Vendor boxes often hide the LCN; fill it in manually if you know it |
| `uri` | What gets passed to `TvView.tune()`. `content://android.media.tv/channel/<id>` for standard rows, or a vendor feeder URI |
| `epgUrl` | Optional XMLTV feed (plain or .gz) used as EPG fallback |

Vendor URI formats seen so far:

- `content://android.media.tv/channel/<id>` — standard TIF rowid
- `dvb://<onid>.<tsid>.<sid>` — DVB triplet, hex
- `/mnt/dtvfs/live/dvb:<onid>.<tsid>.<sid>` — Technicolor dtvfs feeder

## The dump/generation script

`scripts/tv_channels_dump.py` produces `tv_channels.json` over adb.
Two subcommands:

### scan — standard Android TVs

```bash
./scripts/tv_channels_dump.py scan \
    --epg-url "https://epgshare01.online/epgshare01/epg_ripper_IT1.xml.gz" \
    --push
```

- Enumerates `TvInputService`s via `dumpsys tv_input`
- Reads `content://android.media.tv/channel` (id, input_id, name,
  display_number, service_type)
- Skips passthrough inputs (HDMI), deduplicates, sorts by LCN
- Emits standard `content://` URIs and optionally pushes the file to
  the app's external-files dir

If the provider is empty/locked the script prints the manual template
and hints instead of failing.

### harvest — vendor-locked boxes

Vendor input services often log every channel resolution at debug
level while their own app browses the lineup. Technicolor's
`DtvInputService` prints a complete row per service:

```
DtvUriManager: ComputeFeederUri,
  app uri:content://android.media.tv/channel/109845,
  feeder uri:/mnt/dtvfs/live/dvb:110.3a2.bef,
  name:Mediaset Extra HD, type:TYPE_DVB_T,
  data:{...,"scrambled":false,"type":"SERVICE_TYPE_AUDIO_VIDEO"}
```

This is how the TIM sidecar was originally built (161 services in the
log → 87 named → ~70 after dedup). To reproduce:

```bash
./scripts/tv_channels_dump.py harvest --seconds 120 \
    --input-id "com.technicolor.android.dtvinput/.DtvInputService" \
    --epg-url "<feed>" --push
# then, on the box, open the vendor TV app and browse its channel list
```

The capture window runs while you browse/zap on the device; the
parser extracts `(app uri, feeder uri, name, type, scrambled)` per
line, drops unresolved probes (`name:null`), keeps audio+video
services, dedups by feeder URI then by name (`--keep-dupes` keeps
regional duplicates). Use `--pattern` for a different vendor's log
signature.

### Options (both modes)

| Flag | Default | Meaning |
|---|---|---|
| `-s/--serial` | — | adb device serial |
| `--pkg` | `com.moviango.mediaplayer` | package owning the sidecar dir |
| `--epg-url` | — | XMLTV URL embedded as `epgUrl` |
| `--push` | off | `adb push` the result to the device |
| `-o/--out` | `tv_channels.json` | output path |
| `--pattern` | `ComputeFeederUri` | (harvest) logcat substring |
| `--seconds` | 90 | (harvest) capture window |
| `--input-id` | — | (harvest) inputId stamped on rows |
| `--keep-dupes` | off | (harvest) keep same-name duplicates |

## EPG

Priority order:

1. `TvContract.Programs` — only when the channel rows are readable
   (your own input, or a privileged build). Almost never on vendor
   boxes.
2. XMLTV feed — `epgUrl` from the sidecar, cached once per run and
   shared by both TV pages.

Each channel's panel shows the programme currently on air first,
then the following ~24 h. The guide is matched by channel name —
keep sidecar names close to the feed's display names for best hits.

## Linux / MGOS (DVB)

With a DVB adapter present, "Live TV" opens the scanned channel list:

- First visit (empty list): select **Scan channels** — all adapters
  are swept; results persist in kvstore.
- Channels sort by LCN (parsed from the NIT `logical_channel`
  descriptor during the scan) and show now/next programme rows via
  the shared `tvchannel` list item.
- Playback goes through `dvbts://<adapter>/<onid>.<tsid>.<sid>` —
  the TS enters the normal media pipeline: media info, track
  selection, timeshift-free live playback.
- MGOS appliance images ship this by default.

### EPG on DVB — broadcast EIT

No external feed needed: the multiplex itself carries the guide in
EIT sections on PID `0x12`. While a channel is tuned, a second demux
section filter feeds `internal/dvb`'s EIT parser (`ParseEIT` —
present/following `0x4e`/`0x4f` and schedule `0x50`–`0x6f` tables,
short/extended event descriptors, genre and parental rating). Events
merge into a shared store keyed by `onid.tsid.sid` — so tables for
*other* transport streams enrich the whole lineup while you watch —
and the channel list shows now/next from whatever has been collected
(first playback on each mux starts populating its channels).

`internal/dvb/eit_test.go` covers the parser with synthetic sections;
the collector lives in `dvbService.eitCollect`
(`internal/backend/core/platform_dvb_linux.go`).

## Media info on Android

Because playback bypasses the media pipeline, `media.current` is
populated from `TvTrackInfo` instead: source "Live TV", channel name,
input id, resolution/codec type, frame rate, audio language/channels
— whatever the input service reports. It appears in the usual media
info overlay.

## Limitations

- **No recording / CAM / CI**, no multi-tuner management.
- On Linux/DVB the guide fills in as you watch — EIT only arrives on
  the tuned transport stream (plus whatever other-TS tables the
  broadcaster sends), so the channel list is richest after zapping
  around. No background tuner sweep is done to protect the single
  frontend.
- Channel LCN and EPG depend on what the vendor exposes; on walled
  boxes both come from the sidecar + XMLTV.
- Vendor feeder URIs are device-specific — a sidecar is not portable
  between different boxes without re-harvesting.
- On phones/tablets without a `TvInputService` the list shows only a
  "Set up TV channels" entry that launches the native scan activity
  (`tvinput:setup`).

## Troubleshooting

- **Empty channel list** → check `Tv.listChannels` in logcat; if the
  provider is empty, drop a sidecar (script `harvest` mode).
- **Channels listed but won't tune** → verify the `uri` format the
  input expects (`adb logcat | grep ComputeFeederUri` while tuning
  the same channel from the vendor app shows the exact feeder URI).
- **Black video, UI visible** → the vendor surface is behind GLW; if
  the global background is opaque the video is covered — the TV pages
  already disable it via `fullwindow`.
- **Wrong clock / EPG times** → Movian reads `persist.sys.timezone`
  at JNI load; a stale timezone on the box shifts EPG times.
- **Audio keeps playing after leaving Live TV** → fixed: the session
  is released on page deselect, not only on page destroy (navigator
  keeps pages alive for Forward).
