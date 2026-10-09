# Raw screenshot endpoint

## Goal

Return the current Movian UI capture as image bytes for consumers such as mdev. “Raw” means the encoded image response itself, not a pixel buffer and not an Imgur URL.

## HTTP contract

- `GET /api/screenshot/raw` requests a raw capture.
- `GET /api/screenshot?raw=1` and `?raw=true` are equivalent aliases.
- Success is `200 OK`, `Content-Type: image/png`, and a decodable PNG body containing the captured UI frame.
- The body is the PNG bytes directly: no redirect, JSON wrapper, or external upload. The existing HTTP response writer supplies `Content-Length`.
- A raw request must not call Imgur.

mdev’s `take_shot` consumes `/api/screenshot/raw`, checks for a non-empty image signature, hashes the response bytes, and writes those bytes unchanged. Its screenshot probe also requests the query alias.

## Capture and request lifecycle

- A request is carried through `EVENT_MAKE_SCREENSHOT` to the GLW readback that answers it; a frame from another request must never satisfy it.
- Only one HTTP screenshot request may be active at a time. A competing request receives `502` until the active request completes or times out.
- Raw capture has a five-second deadline. If no response is completed by then, return `504`.
- An unsupported capture or encoding failure returns `500`.
- Completion and timeout are mutually exclusive. A late capture from an expired request is ignored and cannot answer a newer request.
- GLW releases its readback pixmap after the delivery callback. Any asynchronous encoding must use the owned image produced by the application adapter, not retain the pixmap.

## Existing behavior to preserve

- `GET /api/screenshot` without a raw selector keeps its JPEG upload and redirects to the Imgur image page `https://imgur.com/{id}`, using the API’s `data.id` rather than the direct image URL in `data.link`.
- A keyboard screenshot continues to save `screenshot.png` in the configured persistent directory.
- Authentication, Imgur configuration, and unrelated screenshot encoding behavior are unchanged.

## Non-goals

Raw pixel/RGBA transport, image-format negotiation, queuing concurrent requests, changes to Imgur upload format or credentials, and screenshot authentication changes.

## Verification

Deterministic behavior checks cover the HTTP handler and its screenshot-event delivery seam: both raw aliases, PNG bytes and MIME type, no-frame timeout, competing requests, late-frame isolation, and a successful legacy upload redirect to `https://imgur.com/{id}`. A separate live GUI smoke requests `/api/screenshot/raw` from the built Movian-Go process and verifies the PNG signature and dimensions; mdev’s consumer path is exercised when available.
