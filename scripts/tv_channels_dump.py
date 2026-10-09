#!/usr/bin/env python3
"""Dump TV channels from an Android device and generate tv_channels.json.

The sidecar format is what Tv.java's mergeDiscoveredChannels() reads at
<sdcard>/Android/data/<pkg>/files/tv_channels.json:

    {"channels": [{"id", "inputId", "name", "number", "uri"}, ...],
     "epgUrl": "<optional xmltv feed>"}

Strategy, best-effort in order:

  1. TvContract.Channels via `content query` — works on standard
     Android TVs where the input app publishes channels (Sony, Philips,
     TCL, Google TV, Live Channels...).
  2. `dumpsys tv_input` — enumerates TvInputServices; passthrough
     inputs (HDMI) are filtered out, the rest is reported so entries
     can be completed by hand when the provider is package-locked
     (e.g. Technicolor TIM box — vendor keeps the DB private).

  harvest mode — vendor boxes often log channel resolution at debug
  level while their own app browses the lineup. Technicolor's
  DtvInputService prints a full row per channel:

    DtvUriManager: ComputeFeederUri,
      app uri:content://android.media.tv/channel/109845,
      feeder uri:/mnt/dtvfs/live/dvb:110.3a2.bef,
      name:Mediaset Extra HD, type:TYPE_DVB_T,
      data:{"name":"Mediaset Extra HD","scrambled":false,
            "type":"SERVICE_TYPE_AUDIO_VIDEO",...}

  So: clear logcat, let the vendor app iterate its lineup (browse its
  channel list, or zap), then harvest the log — that's how the TIM
  sidecar was originally built (161 services -> 76 deduped).

Usage:
    tv_channels_dump.py scan  [-s SERIAL] [--epg-url URL] [--push] [-o OUT]
    tv_channels_dump.py harvest [-s SERIAL] [--pattern REGEX]
                              [--seconds N] [--epg-url URL] [--push] [-o OUT]
"""

import argparse
import json
import re
import subprocess
import sys
import time

CHANNEL_URI = "content://android.media.tv/channel"
PROJECTION = ("_id:input_id:display_name:display_number:"
              "service_type:service_id:network_id:transport_stream_id")


def adb(serial, *args, check=True):
    cmd = ["adb"]
    if serial:
        cmd += ["-s", serial]
    cmd += list(args)
    r = subprocess.run(cmd, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)}: {r.stderr.strip()}")
    return r.stdout


def parse_content_rows(out):
    """Parse `content query` output: 'Row: 0 _id=1, key=val, ...'."""
    rows = []
    for line in out.splitlines():
        line = line.strip()
        m = re.match(r"Row:\s+\d+\s+(.*)", line)
        if not m:
            continue
        row = {}
        # values may contain '=' inside quoted strings — split on
        # ', key=' boundaries only
        for kv in re.split(r",\s*(?=[A-Za-z_][\w]*=)", m.group(1)):
            if "=" in kv:
                k, v = kv.split("=", 1)
                row[k.strip()] = v.strip()
        rows.append(row)
    return rows


def tv_inputs(serial):
    """inputMap entries from dumpsys tv_input -> [inputId]."""
    try:
        out = adb(serial, "shell", "dumpsys", "tv_input", check=False)
    except Exception:
        return []
    inputs = []
    for m in re.finditer(r"^\s{4}(\S+/\S+):\s+info:", out, re.M):
        inputs.append(m.group(1))
    return inputs


def query_channels(serial):
    """TvContract.Channels rows, or [] when the provider is walled."""
    out = adb(serial, "shell", "content", "query", "--uri", CHANNEL_URI,
              "--projection", PROJECTION, check=False)
    return parse_content_rows(out)


DEFAULT_PATTERN = "ComputeFeederUri"


def harvest_logcat(serial, pattern, seconds):
    """Collect logcat while the device iterates its channel lineup."""
    adb(serial, "logcat", "-c", check=False)
    print(f"harvesting {seconds}s — browse the vendor app's channel "
          "list or zap through channels on the box now...")
    time.sleep(seconds)
    out = adb(serial, "logcat", "-d", check=False)
    return [l for l in out.splitlines() if pattern in l]


def parse_feeder_line(line):
    """'DtvUriManager: ComputeFeederUri, app uri:<u>, feeder uri:<u>,
    name:<n>, type:<t>, data:<json>' -> channel dict or None."""
    def field(name):
        m = re.search(name + r" uri:([^,]+)", line)
        return m.group(1).strip() if m else ""
    app_uri = field("app")
    feeder = field("feeder")
    name = ""
    m = re.search(r"name:([^,]+)", line)
    if m:
        name = m.group(1).strip()
    svc_type = ""
    m = re.search(r"type:([^,]+)", line)
    if m:
        svc_type = m.group(1).strip()
    # data:{...} carries scrambled flag + service type + number
    data = {}
    m = re.search(r"data:(\{.*\})\s*$", line)
    if m:
        try:
            data = json.loads(m.group(1))
        except Exception:
            pass
    cid = 0
    m = re.search(r"/channel/(\d+)", app_uri)
    if m:
        cid = int(m.group(1))
    uri = feeder or (f"{CHANNEL_URI}/{cid}" if cid else "")
    if not uri or not name or name.lower() == "null":
        # Unresolved row — the vendor probed a channel id that has no
        # service behind it; skip.
        return None
    return {
        "id": cid,
        "inputId": "",  # filled by caller
        "name": name,
        "number": str(data.get("number", "") or ""),
        "uri": uri,
        "_type": svc_type or data.get("type", ""),
        "_scrambled": bool(data.get("scrambled", False)),
    }


def cmd_harvest(args):
    lines = harvest_logcat(args.serial, args.pattern, args.seconds)
    print(f"log lines matching '{args.pattern}': {len(lines)}")

    by_uri = {}
    for l in lines:
        ch = parse_feeder_line(l)
        if ch is None:
            continue
        # Dedup by feeder URI first — a name seen under several app
        # ids keeps its first row.
        key = (ch["uri"], ch["name"])
        by_uri.setdefault(key, ch)

    chans = list(by_uri.values())
    # Regional duplicates (same service name on different triplets)
    # collapse unless --keep-dupes: keeps the first feeder URI seen.
    if not args.keep_dupes:
        by_name = {}
        for c in chans:
            by_name.setdefault(c["name"].strip().lower(), c)
        chans = list(by_name.values())
    # Skip data-only services and scrambled (unplayable) ones unless
    # asked otherwise; keep audio+video.
    usable = [c for c in chans
              if "AUDIO" in c["_type"].upper() or "DVB" in c["_type"].upper()
              or not c["_type"]]
    chans = usable or chans
    for c in chans:
        c.pop("_type", None)
        c.pop("_scrambled", None)
        if not c["inputId"]:
            c["inputId"] = args.input_id or ""
    chans.sort(key=lambda c: (c["number"] or "999999", c["name"]))

    write_doc(args, chans)

    if args.push:
        remote = (f"/sdcard/Android/data/{args.pkg}/files/"
                  "tv_channels.json")
        adb(args.serial, "push", args.out, remote)
        print(f"pushed -> {remote}")


def write_doc(args, chans):
    doc = {"channels": chans}
    if args.epg_url:
        doc["epgUrl"] = args.epg_url
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=2, ensure_ascii=False)
    print(f"wrote {args.out}: {len(chans)} channels")


def cmd_scan(args):
    inputs = tv_inputs(args.serial)
    print(f"tv_input inputs: {len(inputs)}")
    for i in inputs:
        print(f"  {i}")

    rows = query_channels(args.serial)
    print(f"TvContract channels: {len(rows)}")

    chans = []
    seen = set()
    for r in rows:
        name = r.get("display_name", "").strip()
        cid = r.get("_id", "")
        input_id = r.get("input_id", "")
        stype = r.get("service_type", "")
        if not name or not cid:
            continue
        # Passthrough inputs (HDMI etc.) have no tuneable channels.
        if "PASSTHROUGH" in stype.upper():
            continue
        key = (input_id, name)
        if key in seen:
            continue
        seen.add(key)
        chans.append({
            "id": int(cid),
            "inputId": input_id,
            "name": name,
            "number": r.get("display_number", ""),
            "uri": f"{CHANNEL_URI}/{cid}",
        })
    chans.sort(key=lambda c: (c["number"] or "999999", c["name"]))
    write_doc(args, chans)

    if not chans:
        print("""\
No channels readable via TvContract — the provider is empty or
package-locked (vendor boxes keep their channel DB private). Options:

  * harvest mode: run the vendor app, browse its lineup, capture the
    vendor's own debug log — see 'tv_channels_dump.py harvest --help'
    (this is how the TIM sidecar was originally built)
  * fill the JSON manually per input:

    {"channels": [{"id": <rowid>, "inputId": "<input id above>",
       "name": "<channel>", "number": "<LCN>",
       "uri": "<vendor feeder URI>"}], "epgUrl": "..."}

Vendor URI formats seen so far:
  content://android.media.tv/channel/<id>   (standard TIF rowid)
  dvb://<onid>.<tsid>.<sid>                 (DVB triplet, hex)
  /mnt/dtvfs/live/dvb:<onid>.<tsid>.<sid>   (Technicolor dtvfs feeder)""")

    if args.push:
        remote = (f"/sdcard/Android/data/{args.pkg}/files/"
                  "tv_channels.json")
        adb(args.serial, "push", args.out, remote)
        print(f"pushed -> {remote}")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name, fn in (("scan", cmd_scan), ("harvest", cmd_harvest)):
        p = sub.add_parser(name)
        p.set_defaults(fn=fn)
        p.add_argument("-s", "--serial", help="adb device serial")
        p.add_argument("--pkg", default="com.moviango.mediaplayer",
                       help="app package owning the sidecar dir")
        p.add_argument("--epg-url", default="",
                       help="XMLTV feed URL to embed in the sidecar")
        p.add_argument("--push", action="store_true",
                       help="adb push the result onto the device")
        p.add_argument("-o", "--out", default="tv_channels.json",
                       help="output file (default tv_channels.json)")
        if name == "harvest":
            p.add_argument("--pattern", default=DEFAULT_PATTERN,
                           help="logcat substring to harvest "
                                f"(default '{DEFAULT_PATTERN}')")
            p.add_argument("--seconds", type=int, default=90,
                           help="capture window (default 90)")
            p.add_argument("--input-id", default="",
                           help="inputId to stamp on harvested rows")
            p.add_argument("--keep-dupes", action="store_true",
                           help="keep same-name regional duplicates")
    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as e:
        sys.exit(f"error: {e}")
