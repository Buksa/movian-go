//go:generate go run generate_screenshot_cgo.go

package screenshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
	"unsafe"

	"github.com/czz/movian-go/internal/event"
	"github.com/czz/movian-go/internal/libav"
	httpnet "github.com/czz/movian-go/internal/networking/http"
	"github.com/czz/movian-go/internal/trace"
)

// Codec ID constants (from FFmpeg)
const (
	AVCodecIDMjpeg = 7
	AVCodecIDPng   = 61
	AVPixFmtRGBA   = 26
	SwsBilinear    = 2
)

var imgurClient = http.Client{Timeout: 30 * time.Second}

type screenshotRequest struct {
	conn       *httpnet.HTTPConnection
	raw        bool
	processing bool
	timeout    *time.Timer
}

type ScreenshotHandler struct {
	eventMgr  *event.EventManager
	cachePath string
	imgurID   string
	ts        *trace.TraceSystem

	mu      sync.Mutex
	request *screenshotRequest
}

// NewScreenshotHandler creates a new screenshot handler
func NewScreenshotHandler(eventMgr *event.EventManager, cachePath, imgurID string, ts *trace.TraceSystem) *ScreenshotHandler {
	return &ScreenshotHandler{
		eventMgr:  eventMgr,
		cachePath: cachePath,
		imgurID:   imgurID,
		ts:        ts,
	}
}

// Screenshot handles HTTP screenshot requests
// This is the Go equivalent of hc_screenshot in C.
// /api/screenshot/raw and /api/screenshot?raw=1 return a PNG directly.
func (h *ScreenshotHandler) Screenshot(hc *httpnet.HTTPConnection, remain string, opaque any, method httpnet.HTTPCmd) int {
	rawArg := hc.HTTPArgGetReq("raw")
	raw := remain == "raw" || rawArg == "1" || rawArg == "true"

	h.mu.Lock()
	if h.request != nil {
		h.mu.Unlock()
		return 502
	}

	req := &screenshotRequest{conn: hc, raw: raw}
	h.request = req
	if raw {
		req.timeout = time.AfterFunc(5*time.Second, func() {
			h.fail(req, http.StatusGatewayTimeout, "Screenshot timed out")
		})
	}
	h.mu.Unlock()

	// C: event_to_ui(EVENT_MAKE_SCREENSHOT); GLW delivers the captured pixmap.
	if h.eventMgr != nil {
		e := h.eventMgr.Create(event.EVENT_MAKE_SCREENSHOT, 0)
		e.SetConcrete(req)
		h.eventMgr.EventToUI(e)
	}
	return 0
}

// Deliver receives an owned image and the event that requested its capture.
// Keyboard captures have no HTTP request; expired HTTP captures are discarded.
func (h *ScreenshotHandler) Deliver(capture *event.Event, img image.Image) {
	req, isHTTP := capture.Concrete().(*screenshotRequest)
	if isHTTP {
		h.mu.Lock()
		if h.request != req || req.processing {
			h.mu.Unlock()
			return
		}
		req.processing = true
		h.mu.Unlock()
	} else {
		req = &screenshotRequest{}
	}
	go h.process(req, img)
}

func (h *ScreenshotHandler) finish(req *screenshotRequest, reply func(*httpnet.HTTPConnection)) {
	h.mu.Lock()
	if h.request != req {
		h.mu.Unlock()
		return
	}
	h.request = nil
	if req.timeout != nil {
		req.timeout.Stop()
	}
	h.mu.Unlock()

	if reply != nil && req.conn != nil {
		reply(req.conn)
	}
}

func (h *ScreenshotHandler) fail(req *screenshotRequest, status int, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if req.conn == nil {
		h.ts.Error("SCREENSHOT", "%s", message)
		return
	}
	h.finish(req, func(conn *httpnet.HTTPConnection) {
		conn.HTTPError(status, "%s", message)
	})
}

func (h *ScreenshotHandler) process(req *screenshotRequest, img image.Image) {
	defer func() {
		if r := recover(); r != nil {
			h.ts.Debug("SCREENSHOT", "panic in process: %v\n%s", r, debug.Stack())
			h.fail(req, http.StatusInternalServerError, "Screenshot capture failed")
		}
	}()

	if img == nil {
		h.fail(req, http.StatusInternalServerError, "Screenshot not supported on this platform")
		return
	}

	h.ts.Trace(trace.TRACE_DEBUG, "Screenshot", "Processing image %d x %d",
		img.Bounds().Dx(), img.Bounds().Dy())

	codecID := AVCodecIDPng
	if !req.raw && req.conn != nil {
		codecID = AVCodecIDMjpeg
	}
	data, err := h.compressWithLibav(img, codecID)
	if err != nil {
		h.fail(req, http.StatusInternalServerError, "Unable to compress image: %v", err)
		return
	}

	if req.raw {
		h.finish(req, func(conn *httpnet.HTTPConnection) {
			conn.HTTPSendReply(http.StatusOK, "image/png", "", "", 0, data)
		})
		return
	}

	if req.conn == nil {
		savePath := filepath.Join(h.cachePath, "screenshot.png")
		if err := os.WriteFile(savePath, data, 0644); err != nil {
			h.fail(req, http.StatusInternalServerError, "Unable to save screenshot: %v", err)
		} else {
			h.ts.Trace(trace.TRACE_INFO, "Screenshot", "Written to %s", savePath)
		}
		return
	}

	imgurURL, err := h.uploadToImgur(data)
	if err != nil {
		h.fail(req, http.StatusInternalServerError, "Imgur upload failed: %v", err)
		return
	}
	h.finish(req, func(conn *httpnet.HTTPConnection) {
		conn.HTTPRedirect(imgurURL)
	})
}

// compressWithLibav compresses the image using libav (FFmpeg 7+)
// This is the Go equivalent of screenshot_compress in C
func (h *ScreenshotHandler) compressWithLibav(img image.Image, codecID int) ([]byte, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Find encoder
	codec := libav.AvcodecFindEncoder(codecID)
	if codec == nil {
		return nil, fmt.Errorf("unable to find encoder for codec ID %d", codecID)
	}

	// Allocate codec context
	ctx := libav.AvcodecAllocContext3(codec)
	if ctx == nil {
		return nil, fmt.Errorf("unable to allocate codec context")
	}
	defer libav.AvcodecFreeContext(ctx)

	// Set codec parameters
	ctx.SetPixFmt(codec.GetPixFmts()[0])
	ctx.SetTimeBase(1, 1)
	ctx.SetSampleAspectRatio(1, 1)
	ctx.SetWidth(width)
	ctx.SetHeight(height)

	// Open encoder
	if err := libav.AvcodecOpen2Encoder(ctx, codec, nil); err != nil {
		return nil, fmt.Errorf("unable to open encoder: %w", err)
	}

	// Allocate output frame
	oframe := libav.AvFrameAlloc()
	if oframe == nil {
		return nil, fmt.Errorf("unable to allocate frame")
	}
	defer libav.AvFrameFree(oframe)

	// Allocate image data for output format
	if err := libav.AvImageAlloc(oframe, width, height, ctx.GetPixFmt(), 1); err != nil {
		return nil, fmt.Errorf("unable to allocate image: %w", err)
	}
	// GLW already delivers an owned, upright NRGBA image. Reuse its pixels
	// instead of allocating and copying another full frame.
	rgba, ok := img.(*image.NRGBA)
	if !ok {
		rgba = image.NewNRGBA(bounds)
		draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	}
	srcPtr := unsafe.Pointer(&rgba.Pix[0])
	srcStride := rgba.Stride

	// Setup sws context for color space conversion (RGB32 → codec format)
	sws := libav.SwsGetContext(width, height, AVPixFmtRGBA,
		width, height, ctx.GetPixFmt(), SwsBilinear)
	if sws == nil {
		return nil, fmt.Errorf("unable to create sws context")
	}
	defer libav.SwsFreeContext(sws)

	// Perform color space conversion using C AVFrame data/linesize pointers
	// SwsScaleSingleSrc constructs the srcSlice[4] array in C memory,
	// avoiding the cgo violation of passing a Go array of Go pointers.
	libav.SwsScaleSingleSrc(sws, srcPtr, srcStride,
		0, height, oframe.GetCDataPtrs(), oframe.GetCLinesizePtrs())

	// Encode frame using FFmpeg 7+ API
	oframe.SetPts(libav.AVNoPTSValue)

	// Send frame to encoder
	if err := libav.AvcodecSendFrame(ctx, oframe); err != nil {
		return nil, fmt.Errorf("avcodec_send_frame failed: %w", err)
	}

	// Receive packet from encoder
	pkt := libav.AvPacketAlloc()
	if pkt == nil {
		return nil, fmt.Errorf("unable to allocate packet")
	}
	defer libav.AvPacketFree(pkt)

	if err := libav.AvcodecReceivePacket(ctx, pkt); err != nil {
		return nil, fmt.Errorf("avcodec_receive_packet failed: %w", err)
	}

	return pkt.GetData(), nil
}

// uploadToImgur sends the JPEG as binary multipart data, avoiding base64
// expansion and URL-encoding copies.
func (h *ScreenshotHandler) uploadToImgur(data []byte) (string, error) {
	var body bytes.Buffer
	body.Grow(len(data) + 512) // JPEG plus the fixed multipart headers/boundaries.
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("image", "screenshot.jpg")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.imgur.com/3/upload", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Client-ID "+h.imgurID)
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := imgurClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Link  string          `json:"link"`
			Error json.RawMessage `json:"error"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("imgur HTTP %d: invalid JSON response: %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		message := http.StatusText(resp.StatusCode)
		if len(result.Data.Error) != 0 {
			if err := json.Unmarshal(result.Data.Error, &message); err != nil {
				message = string(result.Data.Error)
			}
		}
		return "", fmt.Errorf("imgur HTTP %d: %s", resp.StatusCode, message)
	}
	if result.Data.Link == "" {
		return "", fmt.Errorf("imgur HTTP %d: no image link in successful response", resp.StatusCode)
	}
	return result.Data.Link, nil
}

// Register registers the screenshot handler with the HTTP server
// This is the Go equivalent of screenshot_init in C
func (h *ScreenshotHandler) Register(server *httpnet.HTTPServer) {
	if server == nil {
		return
	}
	server.HTTPPathAdd("/api/screenshot", h, h.Screenshot, false)
}
