//go:build android

package libav

/*
#include <jni.h>
#include <android/log.h>
#include <libavcodec/avcodec.h>
#include <libavcodec/jni.h>
#include <libavcodec/mediacodec.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_mediacodec.h>
#include <libavutil/opt.h>
#include <libavutil/frame.h>
#include <libavutil/buffer.h>
#include <libavutil/time.h>
#include <stdlib.h>
#include <string.h>

// copy_codec_context is defined in format.go's cgo block
// (avcodec_parameters_to_context on the stream codecpar).
extern int copy_codec_context(void *dst, void *src);

static const void *ml_ffmc_find_decoder(const char *name) {
    return (const void *)avcodec_find_decoder_by_name(name);
}

static void *ml_ffmc_alloc_ctx(const void *codec) {
    return (void *)avcodec_alloc_context3((const AVCodec *)codec);
}

// Register the JavaVM so the JNI MediaCodec backend is available
// (ndk_codec=-1 auto-selects JNI once a JVM is set; we also force it
// explicitly below because this box's libmediandk NDK shim is broken:
// missing symbols, ENOSYS replies and codec aborts).
static void ml_ffmc_set_jvm(void *vm) {
    av_jni_set_java_vm((JavaVM *)vm, NULL);
}

// Run the pts pipeline in µs: packets are sent with pts in
// AV_TIME_BASE units and pkt_timebase rescales presentationTimeUs
// back to µs on output frames. ndk_codec=0 forces the JNI backend —
// the same android.media.MediaCodec API the old wrapper used.
static void ml_ffmc_configure(void *ctx) {
    AVCodecContext *c = (AVCodecContext *)ctx;
    av_opt_set_int(c->priv_data, "ndk_codec", 0, 0);
    c->pkt_timebase = (AVRational){1, AV_TIME_BASE};
}

static int ml_ffmc_open(void *ctx, const void *codec,
                        int width, int height) {
    AVCodecContext *c = (AVCodecContext *)ctx;
    // Vendor codecs run configure() on their CodecLooper thread and
    // assert on a 0x0 MediaFormat (mid-GOP probes leave codecpar
    // without dimensions). Seed like the old JNI codec did
    // (mcp.Width/Height, 1280x720 fallback); the real size arrives
    // via the decoder's output-format reinit anyway.
    if (width <= 0)
        width = 1280;
    if (height <= 0)
        height = 720;
    if (!c->width)
        c->width = width;
    if (!c->height)
        c->height = height;
    if (!c->coded_width)
        c->coded_width = width;
    if (!c->coded_height)
        c->coded_height = height;
    return avcodec_open2(c, (const AVCodec *)codec, NULL);
}

static void ml_ffmc_free_ctx(void **ctx) {
    avcodec_free_context((AVCodecContext **)ctx);
}

static int ml_ffmc_set_extradata(void *ctx, const void *data, int size) {
    AVCodecContext *c = (AVCodecContext *)ctx;
    if (!data || size <= 0)
        return 0;
    c->extradata = av_mallocz(size + AV_INPUT_BUFFER_PADDING_SIZE);
    if (!c->extradata)
        return AVERROR(ENOMEM);
    memcpy(c->extradata, data, size);
    c->extradata_size = size;
    return 0;
}

static int ml_ffmc_has_extradata(void *ctx) {
    return ((AVCodecContext *)ctx)->extradata != NULL;
}

static void *ml_ffmc_hwdevice_alloc(void) {
    return (void *)av_hwdevice_ctx_alloc(AV_HWDEVICE_TYPE_MEDIACODEC);
}

// Store the Java Surface (global ref) in the device context and init
// it. The device does NOT own the ref (mediacodec_device_free only
// releases native_window) — the caller must DeleteGlobalRef it.
static int ml_ffmc_hwdevice_init(void *devbuf, void *surface) {
    AVBufferRef *ref = (AVBufferRef *)devbuf;
    AVHWDeviceContext *devctx = (AVHWDeviceContext *)ref->data;
    AVMediaCodecDeviceContext *mcctx =
        (AVMediaCodecDeviceContext *)devctx->hwctx;
    mcctx->surface = (jobject)surface;
    return av_hwdevice_ctx_init(ref);
}

static void *ml_ffmc_buffer_ref(void *buf) {
    return (void *)av_buffer_ref((AVBufferRef *)buf);
}

static void ml_ffmc_buffer_unref(void **buf) {
    av_buffer_unref((AVBufferRef **)buf);
}

static void ml_ffmc_set_hwdevice(void *ctx, void *buf) {
    ((AVCodecContext *)ctx)->hw_device_ctx = (AVBufferRef *)buf;
}

static void ml_ffmc_delete_global_ref(void *env, void *ref) {
    (*((JNIEnv *)env))->DeleteGlobalRef((JNIEnv *)env, (jobject)ref);
}

static void *ml_ffmc_frame_buffer(void *frame) {
    return ((AVFrame *)frame)->data[3];
}

static int64_t ml_ffmc_frame_pts(void *frame) {
    return ((AVFrame *)frame)->pts;
}

static int ml_ffmc_frame_format(void *frame) {
    return ((AVFrame *)frame)->format;
}

static int ml_ffmc_frame_width(void *frame) {
    return ((AVFrame *)frame)->width;
}

static int ml_ffmc_frame_height(void *frame) {
    return ((AVFrame *)frame)->height;
}

static int ml_ffmc_release_buffer(void *buffer, int render) {
    return av_mediacodec_release_buffer((AVMediaCodecBuffer *)buffer,
                                        render);
}


static int ml_ffmc_pixfmt_mediacodec(void) {
    return AV_PIX_FMT_MEDIACODEC;
}

// Route av_log to logcat — on Android stderr is /dev/null, without
// this FFmpeg's own diagnostics (configure failures, format parsing)
// are invisible.
static void ml_ffmc_log(void *ptr, int level, const char *fmt,
                        va_list vl) {
    if (level > av_log_get_level())
        return;
    char buf[1024];
    vsnprintf(buf, sizeof(buf), fmt, vl);
    int prio = ANDROID_LOG_DEBUG;
    if (level <= AV_LOG_ERROR)   prio = ANDROID_LOG_ERROR;
    else if (level <= AV_LOG_WARNING) prio = ANDROID_LOG_WARN;
    else if (level <= AV_LOG_INFO)    prio = ANDROID_LOG_INFO;
    __android_log_write(prio, "ffmedia", buf);
}

static void ml_ffmc_install_log(void) {
    static int once;
    if (!once) {
        once = 1;
        av_log_set_level(AV_LOG_VERBOSE);
        av_log_set_callback(ml_ffmc_log);
    }
}
*/
import "C"
import (
	"runtime"
	"unsafe"

	"github.com/czz/movian-go/internal/arch"
	"github.com/czz/movian-go/internal/libav"
	mediacore "github.com/czz/movian-go/internal/media/core"
	decoder "github.com/czz/movian-go/internal/video/decoder"
)

// jniEnv — attached JNIEnv for the calling OS thread
// (C: (*JVM)->GetEnv(JVM, &env, JNI_VERSION_1_6)).
func jniEnv() *C.JNIEnv { return (*C.JNIEnv)(unsafe.Pointer(arch.JNIEnv())) }

// ffmcState — per-codec state for the FFmpeg MediaCodec decoder.
// The AVCodecContext itself lives in mc.Ctx (freed by the generic
// media_codec deref before mc.Close runs).
type ffmcState struct {
	devbuf     unsafe.Pointer // AVBufferRef* — the MEDIACODEC hwdevice ctx
	surface    unsafe.Pointer // jobject — global ref, freed in Close
	frame      *libav.AVFrame // persistent receive frame
	mp         *mediacore.MediaPipe
	width      int
	height     int
	h264Parser decoder.H264Parser

	// Deferred-open state: *_mediacodec video decoders REQUIRE
	// SPS/PPS in ctx->extradata (they build MediaFormat csd-0/1 at
	// configure). MPEG-TS carries them only in-band, and
	// find_stream_info may finish probing before the first SPS on a
	// mid-GOP tune — so with no extradata we buffer packets, harvest
	// the parameter sets ourselves and open lazily.
	ctx          unsafe.Pointer // raw AVCodecContext* until opened
	codec        unsafe.Pointer // const AVCodec*
	pending      bool           // decoder not yet opened
	dead         bool           // gave up waiting for parameter sets
	preopen      []*mediacore.MediaBuf
	preopenBytes int
	ps           map[int][]byte // NAL type -> Annex-B unit collected
	diagN        int            // frames logged for diagnostics
}

// ffmcDecoderName maps AV_CODEC_ID_* to the FFmpeg *_mediacodec
// decoder name. Video only: the *_mediacodec audio decoders open on
// this box but the codec dies at the first dequeue
// (AMEDIA_ERROR_UNKNOWN) leaving silent playback — lavc audio costs
// negligible CPU and always works.
func ffmcDecoderName(id mediacore.CodecID) string {
	switch id {
	case mediacore.CodecID(C.AV_CODEC_ID_H264):
		return "h264_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_HEVC):
		return "hevc_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_MPEG4):
		return "mpeg4_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_MPEG2VIDEO):
		return "mpeg2_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_VP8):
		return "vp8_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_VP9):
		return "vp9_mediacodec"
	case mediacore.CodecID(C.AV_CODEC_ID_AV1):
		return "av1_mediacodec"
	}
	return ""
}

// ffmcRequiredNALs — the parameter-set NAL types *_set_extradata
// needs in extradata, per codec. H.264 wants SPS+PPS, HEVC
// VPS+SPS+PPS; other mediacodec decoders tolerate empty extradata
// (common_set_extradata only sets csd-0 when present).
func ffmcRequiredNALs(id mediacore.CodecID) []int {
	switch id {
	case mediacore.CodecID(C.AV_CODEC_ID_H264):
		return []int{7, 8}
	case mediacore.CodecID(C.AV_CODEC_ID_HEVC):
		return []int{32, 33, 34}
	}
	return nil
}

// ffmcNALType — NAL unit type from its header byte: H.264 keeps it in
// the low 5 bits, HEVC in bits 1-6 (2-byte header).
func ffmcNALType(id mediacore.CodecID, b byte) int {
	if id == mediacore.CodecID(C.AV_CODEC_ID_HEVC) {
		return int(b>>1) & 0x3f
	}
	return int(b) & 0x1f
}

// ffmcHarvestNALs scans an Annex-B buffer for parameter-set NALs and
// stores the first unit of each type in ps.
func ffmcHarvestNALs(id mediacore.CodecID, data []byte,
	ps map[int][]byte) {
	required := ffmcRequiredNALs(id)
	if len(required) == 0 {
		return
	}
	i := 0
	for i+4 <= len(data) {
		var nalStart int
		switch {
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			nalStart = i + 3
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 &&
			data[i+3] == 1:
			nalStart = i + 4
		default:
			i++
			continue
		}
		if nalStart >= len(data) {
			break
		}
		nalType := ffmcNALType(id, data[nalStart])
		// Unit ends at the next start code (or end of buffer).
		j := nalStart + 1
		for j+3 <= len(data) {
			if data[j] == 0 && data[j+1] == 0 &&
				(data[j+2] == 1 ||
					(j+4 <= len(data) && data[j+2] == 0 &&
						data[j+3] == 1)) {
				break
			}
			j++
		}
		if j > len(data) {
			j = len(data)
		}
		for _, want := range required {
			if nalType == want {
				if _, have := ps[nalType]; !have {
					unit := make([]byte, j-nalStart)
					copy(unit, data[nalStart:j])
					ps[nalType] = unit
				}
				break
			}
		}
		i = j
	}
}

// ffmcExtradataReady reports whether every required parameter set has
// been harvested.
func ffmcExtradataReady(id mediacore.CodecID, ps map[int][]byte) bool {
	for _, want := range ffmcRequiredNALs(id) {
		if ps[want] == nil {
			return false
		}
	}
	return true
}

// ffmcBuildExtradata concatenates the harvested units as Annex-B
// extradata (4-byte start codes). ff_h264/hevc_decode_extradata parse
// the non-avcC form and the decoder wraps each set into csd-0/1.
func ffmcBuildExtradata(id mediacore.CodecID, ps map[int][]byte) []byte {
	var out []byte
	for _, want := range ffmcRequiredNALs(id) {
		if unit := ps[want]; len(unit) > 0 {
			out = append(out, 0, 0, 0, 1)
			out = append(out, unit...)
		}
	}
	return out
}

// ffmcStoreMeta — C: store_metadata (android_video_codec.c:297-314):
// pushes the buffer's meta into the reorder ring and returns the pts
// MediaCodec will hand back as presentationTimeUs.
func ffmcStoreMeta(vd *decoder.VideoDecoder, mb *mediacore.MediaBuf,
	st *ffmcState, mc *mediacore.MediaCodec, data []byte) int64 {
	mbm := &vd.Reorder[vd.ReorderPtr]
	mediacore.CopyMbmFromMb(mbm, mb)
	vd.ReorderPtr = (vd.ReorderPtr + 1) & decoder.VideoDecoderReorderMask

	isBFrame := false
	switch mc.CodecID {
	case mediacore.CodecID(C.AV_CODEC_ID_MPEG4):
		if len(data) > 4 && data[0] == 0x00 && data[1] == 0x00 &&
			data[2] == 0x01 && data[3] == 0xb6 {
			isBFrame = int(data[4])>>6 == 2
		}
	case mediacore.CodecID(C.AV_CODEC_ID_H264):
		if len(data) > 0 {
			st.h264Parser.DecodeData(data)
			isBFrame = st.h264Parser.SliceTypeNOS == decoder.SliceTypeB
		}
	}

	mbm.PTS = vd.InferPTS(mbm, isBFrame)
	return mbm.PTS
}

// ffmcFillFrameInfo — C: fill_frame_info_from_pts
// (android_video_codec.c:317-348): matches the returned pts against
// the reorder ring to recover epoch/usertime/driveclock/duration/skip.
func ffmcFillFrameInfo(fi *mediacore.FrameInfo, vd *decoder.VideoDecoder,
	st *ffmcState, mc *mediacore.MediaCodec, pts int64) (skip, matched int) {
	// DAR — same rules as LibAVDeliverFrame: frame dims scaled by the
	// frame's sample aspect ratio, falling back to the codecpar SAR.
	// MediaCodec surface buffers carry no geometry of their own, and
	// codecpar dims may still be zero when the decoder was deferred
	// — reading them from the decoded frame is the only reliable
	// source for both.
	darNum, darDen := avFrameWidth(st.frame), avFrameHeight(st.frame)
	if darNum <= 0 || darDen <= 0 {
		darNum, darDen = st.width, st.height
	}
	if darNum <= 0 || darDen <= 0 {
		darNum, darDen = 16, 9
	}
	if sarNum := avFrameSARNum(st.frame); sarNum != 0 {
		darNum *= sarNum
		darDen *= avFrameSARDen(st.frame)
	} else if mc != nil && mc.SARNum != 0 {
		darNum *= mc.SARNum
		darDen *= mc.SARDen
	}
	fi.DARNum = darNum
	fi.DARDen = darDen
	fi.PTS = pts

	for i := 0; i < decoder.VideoDecoderReorderSize; i++ {
		mbm := &vd.Reorder[i]
		if mbm.PTS == pts {
			fi.Epoch = mbm.Epoch
			fi.UserTime = mbm.UserTime
			fi.DriveClock = mbm.DriveClock
			fi.Duration = mbm.Duration
			fi.PTS = mbm.PTS
			mbm.PTS = mediacore.PTSUnset
			switch mbm.AspectOverride {
			case 1:
				fi.DARNum, fi.DARDen = 4, 3
			case 2:
				fi.DARNum, fi.DARDen = 16, 9
			}
			if mbm.Flags.Skip {
				return 1, 1
			}
			return 0, 1
		}
	}
	return 0, 0
}

// ffmcReceive — drains avcodec_receive_frame. Surface-mode frames
// (AV_PIX_FMT_MEDIACODEC) wrap a MediaCodec output buffer in
// frame->data[3]; it is released with render=1 after DeliverFrame so
// MediaCodec presents it — the 'SURF' engine only tracks the pts.
func ffmcReceive(mc *mediacore.MediaCodec, st *ffmcState,
	vd *decoder.VideoDecoder, mq *mediacore.MediaQueue) {
	for {
		ret := avcodecReceiveFrame(mc.Ctx, st.frame)
		if ret < 0 {
			break
		}

		if int(C.ml_ffmc_frame_format(st.frame.CPtr())) ==
			int(C.ml_ffmc_pixfmt_mediacodec()) {
			buffer := C.ml_ffmc_frame_buffer(st.frame.CPtr())
			pts := int64(C.ml_ffmc_frame_pts(st.frame.CPtr()))

			var fi mediacore.FrameInfo
			skip, matched := ffmcFillFrameInfo(&fi, vd, st, mc, pts)
			fi.Type = mediacore.FourCCSURF

			if st.diagN < 10 || (matched == 0 && st.diagN < 40) {
				st.diagN++
				lavTS.Debug("Video",
					"ffmc frame pts=%d match=%d epoch=%d dur=%d skip=%d",
					pts, matched, fi.Epoch, fi.Duration, skip)
			}

			if skip != 0 {
				C.ml_ffmc_release_buffer(buffer, 0)
			} else {
				// Same delivery path as every other platform:
				// the SURF engine clock-waits on fi.PTS and — for
				// pts more than ±5s off the audio clock — falls back
				// to the running gvNextpts sequence instead of
				// stalling (glw_video_android.go). The codec just
				// releases the buffer once the engine is done.
				vd.DeliverFrame(&fi)
				C.ml_ffmc_release_buffer(buffer, 1)
			}
		} else {
			// Software frame (decoder fell back to a CPU output
			// format): hand it to the regular lavc delivery path.
			pts := int64(C.ml_ffmc_frame_pts(st.frame.CPtr()))
			var mbm mediacore.MediaBufMeta
			for i := 0; i < decoder.VideoDecoderReorderSize; i++ {
				if vd.Reorder[i].PTS == pts {
					mbm = vd.Reorder[i]
					vd.Reorder[i].PTS = mediacore.PTSUnset
					break
				}
			}
			if !mbm.Flags.Skip {
				LibAVDeliverFrame(vd, vd.MP, mq, mc.Ctx, st.frame,
					&mbm, 0, mc)
			}
		}

		avFrameUnref(st.frame)
	}
}

// ffmcSend — pushes one buffer's payload into avcodec_send_packet.
func ffmcSend(mc *mediacore.MediaCodec, vd *decoder.VideoDecoder,
	st *ffmcState, mb *mediacore.MediaBuf) {
	if mb.Pkt != nil {
		pkt := mb.Pkt
		if err := DRMDecryptPacket(mc, pkt); err != nil {
			lavTS.Error("MediaCodec", "DRM decrypt failed: %s", err)
			return
		}
		// AVPacket-backed buffers carry stream-timebase pts; the
		// codec runs in µs (pkt_timebase == AV_TIME_BASE_Q).
		pts := ffmcStoreMeta(vd, mb, st, mc, nil)
		avPacketSetPTS(pkt, pts)
		avcodecSendPacket(mc.Ctx, pkt)
	} else if len(mb.Data) > 0 {
		pts := ffmcStoreMeta(vd, mb, st, mc, mb.Data)

		pkt := avPacketAlloc()
		if pkt != nil {
			defer avPacketFree(pkt)
			if avNewPacket(pkt, unsafe.Pointer(&mb.Data[0]),
				len(mb.Data)) >= 0 {
				avPacketSetPTS(pkt, pts)
				avPacketSetDTS(pkt, mb.DTS)
				avPacketSetDuration(pkt, mb.Duration)
				flags := 0
				if mb.Flags.Keyframe {
					flags |= 1 // AV_PKT_FLAG_KEY
				}
				avPacketSetFlags(pkt, flags)
				avcodecSendPacket(mc.Ctx, pkt)
			}
		}
	}
}

// ffmcPending — decode path while the decoder waits for SPS/PPS:
// buffers packets, harvests parameter sets from the Annex-B stream
// (what lavf's extract_extradata does inside the probe window, here
// unbounded) and opens once complete. Packets before the first SPS
// are undecodable anyway, so nothing is lost by holding them.
func ffmcPending(mc *mediacore.MediaCodec, st *ffmcState,
	vd *decoder.VideoDecoder, mq *mediacore.MediaQueue,
	mb *mediacore.MediaBuf) {
	if mb == nil || mb.Flags.Flush || st.dead {
		st.preopen = nil
		st.preopenBytes = 0
		return
	}

	data := mb.Data
	if mb.Pkt != nil {
		data = mb.Pkt.Payload()
	}
	if len(data) == 0 {
		return
	}

	cpy := make([]byte, len(data))
	copy(cpy, data)
	mbc := *mb
	mbc.Data = cpy
	mbc.Size = len(cpy)
	mbc.Pkt = nil // replay goes through the Data branch
	st.preopen = append(st.preopen, &mbc)
	st.preopenBytes += len(cpy)

	if st.ps == nil {
		st.ps = make(map[int][]byte)
	}
	ffmcHarvestNALs(mc.CodecID, cpy, st.ps)

	if !ffmcExtradataReady(mc.CodecID, st.ps) {
		if st.preopenBytes > 16<<20 {
			// ~16MB without parameter sets — either the stream is
			// not Annex-B or it really lacks in-band SPS. Give up:
			// the codec stays closed and packets are dropped.
			lavTS.Error("MediaCodec",
				"no parameter sets in stream, codec never opened")
			st.dead = true
			st.preopen = nil
		}
		return
	}

	extradata := ffmcBuildExtradata(mc.CodecID, st.ps)
	if C.ml_ffmc_set_extradata(st.ctx,
		unsafe.Pointer(&extradata[0]),
		C.int(len(extradata))) < 0 {
		lavTS.Error("MediaCodec", "extradata alloc failed")
		st.dead = true
		st.preopen = nil
		return
	}
	if ret := C.ml_ffmc_open(st.ctx, st.codec,
		C.int(st.width), C.int(st.height)); ret < 0 {
		lavTS.Error("MediaCodec",
			"deferred open failed: %d", int(ret))
		st.dead = true
		st.preopen = nil
		return
	}

	mc.Ctx = libav.WrapAVCodecContext(st.ctx)
	st.ctx = nil
	st.pending = false
	lavTS.Debug("Video",
		"FFmpeg MediaCodec opened after harvesting %d buffered packets",
		len(st.preopen))

	for _, pmb := range st.preopen {
		ffmcSend(mc, vd, st, pmb)
	}
	st.preopen = nil
	st.preopenBytes = 0
	ffmcReceive(mc, st, vd, mq)
}

// ffmcDecode — media_codec decode callback. Replaces the JNI
// queueInputBuffer/dequeueOutputBuffer loop with
// avcodec_send_packet / avcodec_receive_frame.
func ffmcDecode(mc *mediacore.MediaCodec, vdi any,
	mq *mediacore.MediaQueue, mb *mediacore.MediaBuf, reqsize int) {
	st, _ := mc.Opaque.(*ffmcState)
	vd, _ := vdi.(*decoder.VideoDecoder)
	if st == nil || vd == nil {
		return
	}

	if st.pending {
		ffmcPending(mc, st, vd, mq, mb)
		return
	}
	if st.dead || mc.Ctx == nil {
		return
	}

	if mb == nil {
		// C: EOS — feed a NULL packet and drain.
		avcodecSendPacket(mc.Ctx, nil)
		ffmcReceive(mc, st, vd, mq)
		return
	}

	if mb.Flags.Flush {
		// C: a flush-tagged buffer drains the decoder before the next
		// segment's packets, then resets it for reuse.
		avcodecSendPacket(mc.Ctx, nil)
		ffmcReceive(mc, st, vd, mq)
		avcodecFlushBuffers(mc.Ctx)
		return
	}

	ffmcSend(mc, vd, st, mb)
	ffmcReceive(mc, st, vd, mq)
}

// ffmcFlush — C: android_codec_flush → MediaCodec.flush.
// avcodec_flush_buffers forwards to ff_mediacodec_dec_flush which
// flushes the codec and releases pending output buffers.
func ffmcFlush(mc *mediacore.MediaCodec, vdi any) {
	if mc == nil {
		return
	}
	if st, _ := mc.Opaque.(*ffmcState); st != nil && st.pending {
		st.preopen = nil
		st.preopenBytes = 0
		return
	}
	if mc.Ctx != nil {
		avcodecFlushBuffers(mc.Ctx)
	}
}

// ffmcClose — frees the codec's own resources. mc.Ctx is already gone:
// the generic media_codec deref frees it before calling mc.Close; a
// never-opened context (pending) is freed here instead.
func ffmcClose(mc *mediacore.MediaCodec) {
	st, _ := mc.Opaque.(*ffmcState)
	if st == nil {
		return
	}
	if st.ctx != nil {
		C.ml_ffmc_free_ctx(&st.ctx)
	}
	if st.frame != nil {
		libav.AvFrameFree(st.frame)
		st.frame = nil
	}
	// The device ctx does not own the surface global ref — delete it
	// after the decoder is gone.
	if st.devbuf != nil {
		C.ml_ffmc_buffer_unref(&st.devbuf)
		st.devbuf = nil
	}
	if st.surface != nil {
		if env := jniEnv(); env != nil {
			C.ml_ffmc_delete_global_ref(unsafe.Pointer(env),
				st.surface)
		}
		st.surface = nil
	}
	mc.Opaque = nil
}

// ffmcOpen — media_codec open callback. Picks the FFmpeg
// *_mediacodec decoder, binds the app's Surface as ANativeWindow
// through an AVMediaCodecDeviceContext and forces the NDK backend.
func ffmcOpen(mc *mediacore.MediaCodec, mcp *mediacore.MediaCodecParams,
	mp *mediacore.MediaPipe) int {
	// arch.JVM is populated by JNI_OnLoad — which runs after Go init
	// — so register it lazily here; av_jni_set_java_vm is idempotent.
	C.ml_ffmc_set_jvm(unsafe.Pointer(arch.JVM))

	if mp.Sys.VS.VideoAccel == 0 {
		lavTS.Debug("Video", "MediaCodec skipped: VideoAccel setting is 0")
		return 1
	}

	name := ffmcDecoderName(mc.CodecID)
	if name == "" {
		lavTS.Debug("Video", "MediaCodec skipped: codec_id=%d not in list",
			int(mc.CodecID))
		return 1
	}

	cname := C.CString(name)
	codec := C.ml_ffmc_find_decoder(cname)
	C.free(unsafe.Pointer(cname))
	if codec == nil {
		lavTS.Debug("Video", "MediaCodec skipped: %s not built", name)
		return 1
	}

	ctx := C.ml_ffmc_alloc_ctx(codec)
	if ctx == nil {
		lavTS.Debug("Video", "MediaCodec skipped: alloc_context3 failed")
		return 1
	}
	fail := func(what string) int {
		lavTS.Debug("Video", "MediaCodec skipped: %s", what)
		C.ml_ffmc_free_ctx(&ctx)
		return 1
	}

	// Same codecpar/extradata plumbing as MediaCodecCreateLavc —
	// the mediacodec decoder parses avcC/hvcC extradata itself and
	// applies mp4toannexb internally for AVC input.
	if mc.FmtCtx != nil {
		C.copy_codec_context(ctx, mc.FmtCtx.CPtr())
	}
	if mcp != nil && len(mcp.ExtraData) > 0 {
		if C.ml_ffmc_set_extradata(ctx,
			unsafe.Pointer(&mcp.ExtraData[0]),
			C.int(len(mcp.ExtraData))) < 0 {
			return fail("extradata alloc failed")
		}
	}

	C.ml_ffmc_configure(ctx)

	// Output surface — same 'SURF' handshake the JNI codec used:
	// MediaPipe.SetVideoCodec blocks until the Java renderer hands
	// back a Surface, returned here as a jobject.
	var fi mediacore.FrameInfo
	w, h := 0, 0
	if mcp != nil {
		w, h = mcp.Width, mcp.Height
	}
	fi.DARNum = w
	fi.DARDen = h
	if mc.SARNum != 0 {
		fi.DARNum *= mc.SARNum
		fi.DARDen *= mc.SARDen
	}
	fi.Height = h
	surface := 0
	if mp.SetVideoCodec != nil {
		surface = mp.SetVideoCodec(mediacore.FourCCSURF, mc,
			mp.VideoFrameOpaque, &fi)
	}
	if surface <= 0 {
		return fail("no output surface")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	env := jniEnv()
	if env == nil {
		return fail("no JNIEnv")
	}

	// SetVideoCodec hands back a Surface that is already a global ref
	// (ml_vw_surf_job); we adopt it and must DeleteGlobalRef it once.
	gsurface := unsafe.Pointer(uintptr(surface))

	st := &ffmcState{surface: gsurface, width: w, height: h, mp: mp}

	st.devbuf = C.ml_ffmc_hwdevice_alloc()
	if st.devbuf == nil {
		C.ml_ffmc_delete_global_ref(unsafe.Pointer(env), gsurface)
		return fail("hwdevice_ctx_alloc failed")
	}
	if ret := C.ml_ffmc_hwdevice_init(st.devbuf, gsurface); ret < 0 {
		lavTS.Debug("Video", "MediaCodec skipped: hwdevice init %d",
			int(ret))
		C.ml_ffmc_delete_global_ref(unsafe.Pointer(env), gsurface)
		C.ml_ffmc_buffer_unref(&st.devbuf)
		return fail("hwdevice_ctx_init failed")
	}

	C.ml_ffmc_set_hwdevice(ctx, C.ml_ffmc_buffer_ref(st.devbuf))

	st.frame = libav.AvFrameAlloc()
	if st.frame == nil {
		C.ml_ffmc_delete_global_ref(unsafe.Pointer(env), gsurface)
		C.ml_ffmc_buffer_unref(&st.devbuf)
		return fail("frame alloc failed")
	}

	// Deferred open: h264/hevc need SPS/PPS in extradata; a mid-GOP
	// tune can leave codecpar without them. Keep the ctx in st,
	// buffer packets in Decode and open once harvested.
	if len(ffmcRequiredNALs(mc.CodecID)) > 0 &&
		C.ml_ffmc_has_extradata(ctx) == 0 {
		st.ctx = ctx
		st.codec = codec
		st.pending = true
		mc.Opaque = st
		mc.Decode = ffmcDecode
		mc.Flush = ffmcFlush
		mc.Close = ffmcClose
		lavTS.Debug("Video",
			"MediaCodec: %s deferred until in-band parameter sets", name)
		return 0
	}

	if ret := C.ml_ffmc_open(ctx, codec, C.int(w), C.int(h)); ret < 0 {
		lavTS.Debug("Video", "MediaCodec skipped: open %s failed: %d",
			name, int(ret))
		C.ml_ffmc_delete_global_ref(unsafe.Pointer(env), gsurface)
		C.ml_ffmc_buffer_unref(&st.devbuf)
		return fail("avcodec_open2 failed")
	}

	mc.Ctx = libav.WrapAVCodecContext(ctx)
	mc.Opaque = st
	mc.Decode = ffmcDecode
	mc.Flush = ffmcFlush
	mc.Close = ffmcClose

	lavTS.Debug("Video", "FFmpeg MediaCodec (NDK) opened for %s", name)
	return 0
}

func init() {
	C.ml_ffmc_install_log()
	// C: REGISTER_CODEC(android_codec_init, android_codec_create, 100)
	// — ahead of the lavc fallback (prio 1000).
	mediacore.MediaRegisterCodec(&mediacore.CodecDef{
		Open: ffmcOpen,
		Prio: 100,
	})
}
