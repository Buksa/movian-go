# Movian-Go screenshot API: research, review, and verification

## Scope and evidence

Compared the screenshot implementation against the existing Go implementation, Buksa/movian's `movian6` implementation, the mdev consumer, and official Imgur documentation. The comparison baseline was commit `9db952e6f6e0068be47328f6eb36cbb44a2e6f78`. The original local implementation commit is `19bac78`; this note records source behavior and runtime evidence. `BUILDING.md` was not changed.

**Source facts** below cite implementation or first-party documentation. **Runtime observations** are actual runs on this Linux/WSL workstation on 2026-10-06. External-service limits are reported separately from application behavior.

### Primary sources

- [Buksa/movian screenshot implementation, branch movian6][c-screenshot]: `hc_screenshot`, `hc_screenshot_raw`, `screenshot_raw_complete`, `screenshot_process`, `screenshot_deliver`.
- [Go screenshot handler][go-screenshot]: `Screenshot`, `Deliver`, `finish`, `fail`, `process`, `compressWithLibav`, `uploadToImgur`.
- [Go HTTP registration][go-network]: construction and registration of the screenshot handler, including the existing Imgur Client-ID and port 42000.
- [GLW event dispatch][go-events], [readback/delivery][go-glw], and [application adapter][go-adapter]: the capture-event and image-ownership boundary.
- [Go event metadata][go-event]: `Event.SetConcrete`, `Event.Concrete`, `EventManager.EventToUI`.
- [FFmpeg frame wrapper][go-frame] and [encoder wrapper][go-codec]: allocation, release, and encoded-byte ownership.
- [mdev screenshot capture (`take_shot`, `harness.py:1704`)](https://github.com/Buksa/movian/blob/ff079befb6032edc9160aa889215781d2050cf97/support/devtools/mdevlib/harness.py#L1704) and [agent screenshot probe (`movian_agent.py:208`)](https://github.com/Buksa/movian/blob/ff079befb6032edc9160aa889215781d2050cf97/support/devtools/mdevlib/movian_agent.py#L208).
- [Official Imgur API documentation][imgur-docs], especially authentication, image upload, responses, and rate limits; [official legacy image endpoint reference][imgur-image].

## 1. Public contract

| Request | Successful result | External upload |
| --- | --- | --- |
| `GET /api/screenshot/raw` | HTTP 200, PNG bytes, `image/png` | None |
| `GET /api/screenshot?raw=1` | Same raw behavior | None |
| `GET /api/screenshot?raw=true` | Same raw behavior | None |
| `GET /api/screenshot` | Capture JPEG, upload to Imgur, HTTP 302 to `data.link` | Imgur |

A raw request has a five-second deadline. If no completed image is available by then, it receives HTTP 504. While an HTTP screenshot request is pending or processing, another HTTP screenshot request receives HTTP 502. These aliases and statuses come from `hc_screenshot`/`hc_screenshot_raw` in [upstream C][c-screenshot] and are implemented in the [Go handler][go-screenshot].

mdev consumes image bytes rather than an upload URL: its `take_shot` requests `/api/screenshot/raw`, checks HTTP success and image magic, hashes the bytes, and writes them unchanged. Its agent screenshot command also exercises the query alias. A redirect, JSON envelope, or Imgur URL would therefore be the wrong raw contract. The implementation preserves PNG output even though mdev can recognize other image magic.

Raw and legacy responses use the existing [HTTP response writer][go-http]. `HTTPSendReply` emits the requested content type and delegates to `HTTPSendRaw`, which supplies the content length and the connection's existing keep-alive policy. The raw path does not need a second custom header writer.

The keyboard screenshot path is also existing behavior, not speculative functionality: [GLFW key handling][go-glfw] creates `EVENT_MAKE_SCREENSHOT` for modified F12. A capture without an HTTP request writes `screenshot.png` to the handler's configured directory. The current application passes `persistentPath` to that constructor. This revision restores that behavior rather than removing it under YAGNI. [Sources: adapter and handler][go-adapter].

## 2. Review findings and their fixes

### Spec: late captures could answer the wrong request

The first raw implementation guarded `finish(req, ...)` with request identity but associated a frame with a request only when `Deliver(img)` ran. This left a gap:

1. Request A posts a capture event.
2. A times out before GLW dispatches that event.
3. Request B becomes active.
4. A's delayed frame reaches `Deliver(img)`, which selects B.

The final-completion identity check is too late: the frame has already been assigned to the wrong request. The regression reproduced this exact pattern and observed A's pixel in B's PNG.

**Fix:** use the existing `Event.SetConcrete(req)` metadata seam, forward the originating event through GLW dispatch/readback and the application callback, and check that captured request against `h.request` before starting processing. `finish` retains its identity check to make timeout versus completion exactly-once. No request-ID counter, new queue, or generic job system is required. [Sources: event metadata][go-event], [dispatch][go-events], [readback][go-glw], [adapter][go-adapter], [handler][go-screenshot].

This is a stronger request-correlation invariant than merely distinguishing raw and non-raw capture modes. Upstream C's wait state and raw event flag are useful compatibility references, but a mode flag by itself is not a unique identifier for successive raw requests. [Source: upstream C][c-screenshot].

### Spec: keyboard screenshots were discarded

The first raw implementation returned from `Deliver` whenever no HTTP request was active. That discarded keyboard-originated captures before the pre-existing file-save branch could run. The regression reproduced the missing file.

**Fix:** distinguish request-bearing HTTP events from ordinary keyboard events. An HTTP capture must match its originating request; a keyboard capture receives an independent non-HTTP processing context and saves PNG. It cannot consume the current HTTP request. [Sources: key handling][go-glfw] and [handler][go-screenshot].

### Standards: duplicated error handling and unnecessary wrappers

No hard documented coding-standard violation was identified. The useful heuristic findings were duplicated raw-versus-legacy error branches and middle-man wrappers.

**Fixes:**

- `finish` owns the one HTTP-request release/deadline-stop rule.
- `fail` owns screenshot error reporting; both HTTP modes use the same error response mechanism, while file-capture failures are logged.
- Raw success calls the existing `HTTPSendReply` directly.
- Legacy success redirects directly after upload.
- Application setup calls the handler's existing `Register` method instead of reimplementing registration in a closure.
- Removed the uncalled `Pending` method, unused pixmap-flag interface/flip helper, unused RGB32 constant, the no-op deferred `AvcodecClose`, and one-line save/response wrappers.

The FFmpeg CGO generator and its Makefile drift checks were retained: they are an existing repository convention, not a new abstraction introduced by this feature. [Sources: handler][go-screenshot], [application registration][go-network], [Makefile][makefile].

## 3. Ownership and allocation analysis

### The necessary copy stays

GLW releases its readback pixmap after the delivery callback returns. The [application adapter][go-adapter] creates an independently owned `image.NRGBA` and normalizes the GL readback's channel order and bottom-up rows before returning. The asynchronous encoder therefore does **not** borrow a pixmap that GLW has already released. [Source: GLW readback][go-glw].

Removing that copy without introducing explicit retained pixmap ownership would be unsafe. It was not removed.

### The redundant full-frame copy is removed

The original compressor always allocated an `image.RGBA` and copied the adapter's already-owned image into it. The revised compressor reuses pixels/stride when the input is `*image.NRGBA`; it retains a conversion fallback for other `image.Image` implementations. Vertical flipping remains at the adapter boundary, not in a second optional flag interface that had no callers. [Sources: adapter][go-adapter] and `compressWithLibav` in [handler][go-screenshot].

For the verified 1280 × 720 capture, the eliminated four-byte-per-pixel intermediate was 3,686,400 bytes. This is an allocation/copy removal derived from the image dimensions, **not** a claimed latency benchmark.

FFmpeg still owns its codec/frame/packet resources. `AvImageAlloc` attaches an `AVBufferRef` to the frame so `AvFrameFree` releases the allocation. `AvcodecReceivePacket` copies packet bytes using `C.GoBytes`; the returned Go bytes remain valid after the packet is freed. The existing `AvcodecClose` wrapper is a no-op and `AvcodecFreeContext` performs the actual cleanup, so the screenshot code no longer calls both. [Sources: frame wrapper][go-frame] and [codec wrapper][go-codec].

## 4. Imgur request and response behavior

### Authentication and upload format

Imgur API v3 uses HTTPS. Anonymous/public uploads use `Authorization: Client-ID ...`; account-authorized operations use OAuth bearer authorization. The application already supplies the same Client-ID used by upstream Movian. [Sources: official authentication documentation][imgur-docs], [upstream upload][c-screenshot], [Go registration][go-network].

The old implementation base64-encoded JPEG, URL-encoded that string, and copied the encoded form into a byte slice. Imgur supports binary image input; the revised implementation sends one standard multipart file field named `image` to the existing `/3/upload` endpoint. It preallocates the multipart buffer for the JPEG and its fixed headers/boundaries. This removes base64 expansion and URL-encoding copies without introducing streaming goroutines, an SDK, or a general upload framework. [Sources: official image upload reference][imgur-image] and [Go uploader][go-screenshot]. The live upload below confirms this request format is accepted by the existing endpoint.

A shared HTTP client bounds the upload transaction to 30 seconds rather than leaving the active screenshot gate held indefinitely by an unbounded network call. There are no retries, backoff, credential fallbacks, or hidden second upload. [Source: uploader][go-screenshot].

### Response validation and errors

The legacy endpoint redirects only when the upstream response is HTTP 2xx, `success` is true, and `data.link` is present. A contradictory success envelope on HTTP failure is not treated as a successful upload.

The parser keeps `data.error` as `json.RawMessage`, so it can report a string or structured error without a string-field decoding failure masking the upstream HTTP status. Invalid/incompatible JSON also reports that status. The caller exposes upload failures through the existing screenshot HTTP-error path; it does not turn a failed upload into a successful empty response. [Source: uploader and `fail`][go-screenshot].

Offline regression cases cover string errors, structured errors, non-JSON HTTP failures, HTTP failure with a success envelope, and a successful envelope with no image link. They use a local TLS server and do not consume Imgur quota.

### Deliberately not added

Official docs describe rate-limit headers and `deletehash` for deleting anonymous uploads. The legacy screenshot contract needs only the upload link. This revision does not add automatic deletion, retry policies, quota tracking, stored credentials, or an Imgur SDK. Numeric service quotas may change; runtime 429 responses remain a separate operational constraint. [Sources: official responses/rate limits][imgur-docs] and [image reference][imgur-image].

The screenshot route inherits the existing server exposure. The smoke requests supplied no HTTP authentication. Raw mode avoids public upload, but an accessible screenshot API can still reveal UI contents; it should not be exposed to untrusted networks.

## 5. Runtime observations

### Correcting the earlier startup diagnosis

An immediate request after the HTTP listener appeared returned `504 Screenshot timed out` after **5.037 seconds**. A SIGQUIT dump of the owned test process showed GLW alive in `glwGlfwMainloop`'s sleep branch, and the log showed actual frame preparation. This was not evidence of a blocked mutex or stalled graphics-driver call.

Changing only probe readiness—waiting for the first `UI size scale changed` frame-preparation log—produced a real HTTP 200 PNG in **0.131 seconds**. The HTTP listener comes up before the UI event subscription; sending a capture event before that subscription exists does not replay it after UI initialization. HTTP readiness is therefore not sufficient proof of GLW readiness. No environment-variable workaround or application-level retry was added.

### Final raw smoke

The revised application was launched with a fresh profile, X11, and vendored GLFW 3.4 inside an isolated network namespace with only loopback enabled. All three raw aliases returned PNG without external-network access:

| Endpoint | HTTP | Content type | Dimensions | Bytes | Elapsed seconds |
| --- | --- | --- | --- | --- | --- |
| `/api/screenshot/raw` | 200 | `image/png` | 1280 × 720 | 4348 | 0.283 |
| `/api/screenshot?raw=1` | 200 | `image/png` | 1280 × 720 | 167508 | 0.174 |
| `/api/screenshot?raw=true` | 200 | `image/png` | 1280 × 720 | 225491 | 0.183 |

PNG signature and IHDR dimensions were checked. The captured surface was inspected: the Home screen, title, icons, and folder labels were upright, with correct displayed colors. Different byte counts are from successive frames during UI animation, not a codec/performance comparison.

### Actual legacy upload

`GET /api/screenshot` on the revised live application completed the binary-multipart upload and returned:

- HTTP **302**, `Location: https://i.imgur.com/qdUTX8Z.jpeg`, in **1.695 seconds**.
- An authenticated public image-metadata GET to `https://api.imgur.com/3/image/qdUTX8Z` returned HTTP **200**, `success: true`, ID `qdUTX8Z`, dimensions **1280 × 720**, type **image/jpeg**, and size **52260 bytes**.
- Fetching the direct file from `i.imgur.com` in the WSL smoke returned HTTP **429**. This was a client/CDN download limitation, not evidence of a failed upload or an inaccessible public Imgur page. No CDN retry loop was used.

The user subsequently confirmed that the public image pages [qdUTX8Z](https://imgur.com/qdUTX8Z) and [tIkS3i7](https://imgur.com/tIkS3i7) are accessible. This is user-reported verification, separate from the automated upload/metadata checks above. The legacy redirect continues to use the API's `data.link`; it does not need to rewrite a direct image URL into an Imgur page URL.

A separate live probe with an invalid Client-ID received `imgur HTTP 429: Too Many Requests`. This proves the uploader reports an actual service failure/status, but does **not** establish the service's invalid-credential response: rate limiting prevented that narrower check. The network-dependent probe was removed afterward.

### Regression and build checks

Before the fixes, the targeted regressions failed with:

- `replacement received old capture pixel 0x1212 0x3434 0x5656, want blue`;
- `keyboard screenshot did not save a PNG`.

After revision, `go test -race ./internal/api/screenshot -count=1 -timeout=25s` passed, covering raw aliases/pixels, timeout, overlapping requests, late capture versus replacement, keyboard file capture, and the Imgur error cases.

The GUI build passed using the vendored GLFW path selected by the Makefile:

```sh
PKG_CONFIG_PATH="$PWD/third_party/glfw/linux/lib/pkgconfig" \
  go build -tags 'x11 glfw' -o /tmp/movian-screenshot-review ./cmd/movian-go
```

A plain uncached build selected the installed GLFW 3.3.10 and failed on GLFW 3.4 APIs. Selecting the repository's existing vendored GLFW 3.4 resolved that prerequisite; no build documentation or dependency source was changed.

The repository ignores `*_test.go`, so regression files remain local and were not force-added. Test application processes were stopped; unrelated user processes and skin/FFmpeg changes were not modified. Temporary live/network probes were removed after collecting evidence.

[c-screenshot]: https://github.com/Buksa/movian/blob/movian6/src/api/screenshot.c
[imgur-docs]: https://apidocs.imgur.com/
[imgur-image]: https://api.imgur.com/endpoints/image
[go-screenshot]: ../../internal/api/screenshot/screenshot.go
[go-network]: ../../cmd/movian-go/wire_network.go
[go-adapter]: ../../cmd/movian-go/ui_glw_common.go
[go-events]: ../../internal/ui/glw/events.go
[go-glw]: ../../internal/ui/glw/glw.go
[go-glfw]: ../../internal/ui/glw/glw_glfw.go
[go-event]: ../../internal/event/event.go
[go-frame]: ../../internal/libav/avframe.go
[go-codec]: ../../internal/libav/avcodec_ctx.go
[go-http]: ../../internal/networking/http/http_server.go
[makefile]: ../../Makefile
