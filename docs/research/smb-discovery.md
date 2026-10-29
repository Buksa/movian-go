# SMB server discovery for Movian Go: research and design

## Scope and evidence

This note records why the Local network page found no SMB servers on a modern LAN, the protocol facts the replacement relies on, and the design that was built. It also lists the C Movian bugs that were deliberately not ported and the known limits of the result.

Research snapshot 2026-10-24. Baseline: Movian Go `master` at `a13b944` and C Movian `movian6` at `9b1c9767a`. **Source facts** cite specifications, vendor documentation or pinned source. **Runtime observations** come from a probe-only prototype and the final build run on a real LAN with two Windows 11 PCs, a TrueNAS box (Samba with wsdd and Avahi) and a Linux test host. All addresses here are documentation addresses (192.0.2.0/24) and all host names are generic; real values were removed on purpose. Claims marked *(inference)* are design reasoning that no run has confirmed.

### Primary sources

- [Microsoft Learn: SMBv1 is not installed by default](https://learn.microsoft.com/en-us/windows-server/storage/file-server/troubleshoot/smbv1-not-installed-by-default-in-windows)
- [Samba 4.11 release notes](https://www.samba.org/samba/history/samba-4.11.0.html) and [TrueNAS SMB documentation](https://www.truenas.com/docs/scale/scaletutorials/shares/smb/)
- [OASIS WS-Discovery 1.1](https://docs.oasis-open.org/ws-dd/discovery/1.1/os/wsdd-discovery-1.1-spec-os.html)
- [RFC 6762, Multicast DNS](https://www.rfc-editor.org/rfc/rfc6762.html), §5.1 and §6.7
- [MS-PBSD, Computer Information](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-pbsd/ec0810ba-2427-46f5-8d47-cc94919ee4c1)
- [wsdd source](https://github.com/christgau/wsdd/blob/c7747cc0195ed2ac7c9af2bd4082226a4ed65c1b/src/wsdd.py) (`wsdd.py`, pinned commit `c7747cc`)
- [Kodi WS-Discovery listener](https://github.com/xbmc/xbmc/blob/a169167d30b648db91c0744984be3d790c5e67c5/xbmc/platform/posix/filesystem/SMBWSDiscoveryListener.cpp)
- [Android NsdManager](https://developer.android.com/reference/android/net/nsd/NsdManager) and [Android local network permission](https://developer.android.com/privacy-and-security/local-network-permission)

## 1. Why NetBIOS finds nothing

Movian Go found SMB servers only through NetBIOS: it broadcast a `__MSBROWSE__` name query and asked the master browser for its list over SMBv1 `NetServerEnum2`. A modern network has neither half of that exchange.

- Windows 10 1709 and later do not install SMBv1, and the Computer Browser service goes with it. Explorer now finds hosts through Function Discovery, that is, WS-Discovery.
- Samba 4.11 and later default `server min protocol` to `SMB2_02`. TrueNAS disables `nmbd` by default and points macOS clients at mDNS.

On a typical home LAN the page therefore showed no SMB hosts at all. Live runs against Windows 11 and Samba confirmed it before the work started.

The rest of the baseline mattered for the design: mDNS browsed only `_webdav._tcp` and `_htsp._tcp`; there was no WS-Discovery; nothing de-duplicated services (`serviceCreate0` only renamed on an id clash); `--disable-sd` was parsed but never read; and `smb://` is the single scheme, SMB2 first with an SMBv1 fallback, so one URL form serves every source.

## 2. Protocol facts the design relies on

### A WS-Discovery Probe gets unicast replies

A ProbeMatch is sent by unicast to the source address and port of the Probe ([WS-Discovery 1.1](https://docs.oasis-open.org/ws-dd/discovery/1.1/os/wsdd-discovery-1.1-spec-os.html)). A client can therefore probe from an ephemeral port: it never binds 3702 and never has to receive multicast. Only Hello and Bye arrive by multicast.

*Observed:* on the real LAN every reply (ProbeMatch, ResolveMatch) reached the ephemeral port by unicast, 0.2 to 2.1 s after the Probe; a Get took 5 to 36 ms.

### An mDNS query from a port other than 5353 gets a unicast answer

A query whose source port is not 5353 is a legacy one-shot query, and the responder "MUST send a UDP response directly back to the querier, via unicast", repeating the query ID ([RFC 6762 §6.7](https://www.rfc-editor.org/rfc/rfc6762.html#section-6.7); §5.1 defines the one-shot query). A PTR query for `_smb._tcp.local.` from an ephemeral port needs no 5353 bind, cannot clash with avahi-daemon, mDNSResponder or Windows' own responder, and receives no multicast.

*Observed:* the TrueNAS responder put PTR, TXT, SRV, A and AAAA all in the **answer** section (the additional section was empty), so the parser reads both sections. The TTL was 10 s, the legacy-unicast cap, so the reply TTL cannot drive aging.

### wsdd answers only `Types` = `wsdp:Device`

wsdd, the WS-Discovery daemon Samba hosts and NAS boxes run, answers a Probe only when its Types is exactly `wsdp:Device` (`wsdd.py:983` at the pinned commit). C Movian probes `pub:Computer`: that found both Windows hosts but missed the TrueNAS box. A `wsdp:Device` probe found all three. Windows answers both.

### wsdd's ProbeMatch has no XAddrs

Windows puts the transport address (`XAddrs`, `http://<ip>:5357/<uuid>`) in its ProbeMatch. wsdd's ProbeMatch carries only the endpoint reference (EPR), so the client must multicast a **Resolve** for the EPR; the unicast ResolveMatch comes back within milliseconds and carries the XAddrs. Without this step a wsdd host is known by address only.

### The name needs a WS-Transfer Get

A ProbeMatch carries no host name. The name and workgroup come from a WS-Transfer Get on the XAddr, whose `pub:Computer` value is defined by [MS-PBSD](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-pbsd/ec0810ba-2427-46f5-8d47-cc94919ee4c1) in these forms:

| Value | Meaning |
| --- | --- |
| `NAME/Domain:D` | joined to domain D |
| `NAME\Workgroup:W` | member of workgroup W |
| `NAME\NotJoined` | not joined to anything |

Live Windows and wsdd both send `NAME/Workgroup:WORKGROUP`, with a slash, which the specification does not list. The parser therefore accepts both `/` and `\`. wsdd uses a 2 s HTTP timeout for the Get; Kodi does the same Get and parses the tags by substring.

### Windows repeats ProbeMatches

For each Probe Windows sent its ProbeMatch four times over one to two seconds, and wsdd sent it twice. A round must de-duplicate by EPR, issue one Get per EPR, and Get again only when `MetadataVersion` changes.

### Other facts

- **One host, several sources.** The TrueNAS box shows up in WS-Discovery (`NASBOX`) and in `_smb._tcp` (`nasbox.local.`) with the same address. Windows hosts do not publish `_smb._tcp`.
- **Android.**
  - Wi-Fi drivers drop inbound multicast unless the app holds `WifiManager.MulticastLock`; Android 13 with T extensions 7 and later manages this for foreground apps ([NsdManager](https://developer.android.com/reference/android/net/nsd/NsdManager)).
  - Unicast replies to our own ephemeral socket are ordinary unicast and arrive without the lock. Checked on an Android 16 phone: a plain Go socket received the ProbeMatch replies with no `WifiManager.MulticastLock` held.
  - Android denies the netlink address dump to apps, so the interface list comes from the kernel's choice of source address for a route to the LAN. Limitation: only the default-route LAN is probed there.
  - `ACCESS_LOCAL_NETWORK` is enforced only for apps targeting API 37 and later ([local network permission](https://developer.android.com/privacy-and-security/local-network-permission)); the Movian Go APK targets 28.
- **Go mDNS libraries.** `grandcat/zeroconf` is already a dependency (last commit January 2023, its README lists TTL handling as missing); `hashicorp/mdns` and `brutella/dnssd` are maintained. No maintained Go WS-Discovery client for SMB was found.

## 3. What C Movian did, and what was not ported

C Movian (`src/sd/wsd.c`, Avahi, NetBIOS) sent a Probe for `pub:Computer` every 20 s to `239.255.255.250:3702`, handled only ProbeMatch (no Hello or Bye), took the connect address from the reply's UDP source, and aged out a silent host after about 60 s. IPv4 only. The design keeps that shape and **does not port these defects**:

- **The workgroup parser splits only on `/`.** `wsd_split_computer` mishandles the `\` forms MS-PBSD specifies. The Go parser accepts both separators and `NotJoined`.
- **The Probe type is `pub:Computer`.** It misses every wsdd host. The Go engine probes `wsdp:Device`.
- **No Resolve.** A ProbeMatch without XAddrs left the host unnamed. The Go engine resolves it.
- **No cross-source de-duplication.** One host could appear as both a name and an address. The Go engine merges every source into one entry per host.
- **Probes and sweeps never aged out.** Wildcard NBNS sweep and SMB2-probe entries stayed forever when no master browser answered. Every Go entry ages.
- **One wildcard socket.** C Movian's Probe leaves through the default-route interface only. The Go engine probes every LAN interface (below).
- **A LAN address and a Windows host name in a source comment.** Not carried over, and not carried into this repository.

## 4. Design

One discovery engine (`internal/sd/smbdisc`) owns every SMB host entry. Each source reports an observation, `{source, name, workgroup, address, port}`, and the engine merges them into one service per host. It runs on every platform from the same Go code, IPv4 only.

```text
smbdisc
  WS-Discovery  every 20 s: Probe Types wsdp:Device from an ephemeral port on
                every LAN interface, unicast ProbeMatch, EPR de-dup per round,
                no XAddrs -> Resolve, then Get (2 s) for NAME and workgroup
  mDNS          every 20 s: one-shot PTR _smb._tcp.local. on every interface;
                SRV, A, AAAA from the answer or additional section, follow-up
                queries only for what is missing; TTLs ignored
  NetBIOS       the old path, now only reports servers found and lost
  merge         by address, then by normalized name
  service       id smb/<NAME>, URL smb://<address>[:port], probe off,
                origin "discovered"
  aging         60 s without any source
```

### Sources and rounds

- **WS-Discovery.** Probe and Resolve go out as multicast to `239.255.255.250:3702`; replies return by unicast. At most 4 Gets run at once, a GetResponse is capped at 64 KiB, and a round has at most 16 unanswered Resolves.
- **mDNS.** A round sends one PTR query per interface with a random non-zero ID. The port comes from the SRV record. A per-source cap and a global cap on instances guard against floods.
- **NetBIOS.** Kept as a fallback source. Its manager no longer creates services; it reports servers found and lost. Names are normalized (upper case, first DNS label, at most 15 characters). A server known only by name is listed at `smb://NAME` until another source supplies an address.
- **Rounds.** A round is a Probe plus a 3 s collection window; rounds start every 20 s. The first round starts at once. `Config.NetworkChanges` triggers an immediate round, bursts coalescing into at most one extra.

### Probing every interface

A single `0.0.0.0:0` socket multicasts out of the default-route interface only, so a multi-homed host (Docker, Tailscale, Hyper-V or VMware adapters, Wi-Fi plus mobile data) would miss the LAN. Each round therefore re-reads the interface list and opens one UDP socket per IPv4 address, bound to that address with port 0, with `IP_MULTICAST_IF` set.

An interface is used when it is up, running and multicast-capable, and neither loopback nor point-to-point; `0.0.0.0` is dropped. Every IPv4 address of an interface is used. An address that fails to bind is skipped with a debug trace. All Probes of a round share one MessageID and replies are handled one at a time, so a host heard through two interfaces is taken once. Sockets live for one round only.

### Merge and name precedence

An entry is keyed by address, then by normalized name (upper case, first label, no `.local`). The title comes from the first source that has one, in this order: **WS-Discovery name, NetBIOS name, mDNS name, address.** A name that arrives later renames the entry; a host listed only by address is retried every round.

### Id and URL rules

- **Id** `smb/<NAME>` (upper case), or `smb/<address>` until a name is known. Per-service settings are stored by id, so they survive a change of address.
- **URL** `smb://<address>`, with `:port` only when the port is not 445. The URL carries the address because `.local` and NetBIOS names often do not resolve: Movian Go has no Go mDNS resolver and TrueNAS has NetBIOS name service off. The address that answered is the one that is reachable. A host known only by NetBIOS name is listed at `smb://NAME`.
- Services are created with probing off and origin "discovered", as C Movian's `service_create_managed` call does. A title change creates a new service under the same id when needed; a changed address only updates the URL.

### Aging

An entry goes when no source has seen it for 60 s, which is three missed rounds, checked as each round's collection window closes. A host that disappears is therefore gone within about 60 s, and one that returns is back within one round (about 20 s). A host NetBIOS still lists stays by name when its address is lost. The mDNS TTL (10 s for a legacy answer) is ignored.

### Stop and `--disable-sd`

The engine starts only when service discovery is enabled, and `--disable-sd` now gates it. With the flag the Go port also skips NetBIOS host discovery and the mDNS/Bonjour backends; NetBIOS **name resolution** stays on. `Stop` cancels the engine's context, waits for the rounds to end and unsubscribes from network-change notifications; a sighting being published as Stop is called either is published first or is dropped. Checked on the final build: with `--disable-sd` the process emitted no `SMBDISC` trace line and bound no 5353 socket.

### Decisions

1. **Mechanisms:** WS-Discovery plus one-shot mDNS `_smb._tcp`; NetBIOS stays as a fallback source.
2. **Sockets:** probe only, from ephemeral ports, reading only unicast replies. No Hello or Bye, no JNI, no MulticastLock.
3. **Identity:** one entry per host, merged by address and then by name; id by name, URL by address.
4. **Aging:** 60 s, not the reply TTL.

The price of probe-only is that Hello and Bye are never seen, so a host appears or disappears within a round instead of at once. C Movian lived with the same limit.

## 5. Known behaviour and limits

- **On Windows the PC running Movian Go does not list itself.** Windows WS-Discovery ignores multicast Probes sent from its own host and answers only unicast ones. Checked with a plain .NET `UdpClient` in PowerShell, independent of Movian Go: a unicast Probe from the PC to its own port 3702 got four ProbeMatches, a multicast Probe from the same PC got answers from the other hosts and never from itself. Other hosts on the LAN see that PC normally. Listing the local host would take an extra unicast Probe to each own address; the behaviour is kept.
- **Desktop network changes do not trigger an immediate round.** The ifaddr network monitor is not started in production and the Go port has no netlink hook, so on desktop platforms only the 20 s timer drives rounds. Android's default-network callback and asyncio's own start do raise the notification. The engine and its tests handle the notification; the missing piece is the source of it on desktops. A host that appears is found within one round (20 s), and the interface list is re-read each round anyway.
- **IPv6 is out of scope.** No `FF02::C` or `FF02::FB` queries; C Movian did not have them either. IPv6-only hosts are not found. A host that publishes an AAAA record in the answer is still listed by its IPv4 address.
- **Hello and Bye are not handled** (see above).
- **Android.** Unicast replies reach a plain Go socket without the MulticastLock (see above), so nothing is held. Only the default-route LAN is probed, because Android denies the netlink dump. An emulator behind NAT cannot test any of this; a real phone on the LAN did.
- **Probes run only while Local network is the current page.** The engine's rounds, and the network-change rounds, wait until a page holds `Watch`, so nothing is sent while another page is current.
- **Avahi.** Avahi's own legacy-unicast answer was not observed separately from TrueNAS's responder. RFC 6762 §6.7 makes the unicast reply a MUST, and the follow-up queries cover a reply without SRV or A.
- **Synology** and other NAS vendors: whether they register `_smb._tcp` is unverified. They are found through WS-Discovery or NetBIOS when they run them.
- **darwin** was compile-checked only; no runtime check.
- **Not tested:** a host with two IPv4 addresses on one interface (covered by a unit test only).

## 6. Verification

- **Automated:** parser fixtures (sanitized captures from a Windows host and a wsdd host, plus truncated and hostile XML), merge and aging on a fake clock, and loopback WS-Discovery and mDNS responders. They run anywhere and use no LAN.
- **LAN run** (`TestLANSmoke`, enabled by `SMBDISC_SMOKE=1`): runs the real engine on the host's interfaces for about 30 s and prints the Local network entries. It must run on a Linux host that does not share an address with a Windows target; a probe from a host whose address is itself a target proves nothing.
- **Results:** every server that was on appeared exactly once and by name: both Windows PCs, and the TrueNAS box as `NASBOX` (a wsdd host named through Resolve and Get, and seen by mDNS too). On Windows the local PC was absent, as described above. Probes left every multicast-capable interface (Ethernet, VMware and VirtualBox adapters, Wi-Fi) with one MessageID; VPN and disconnected adapters were skipped.
