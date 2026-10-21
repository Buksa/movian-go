// Go extension (no C counterpart): fMP4/CMAF HLS segment demuxer.
//
// Segments whose probe bytes fail the MPEG-TS sync check and that carry
// an EXT-X-MAP URI — or start with an ISO-BMFF box signature — are fed
// to FFmpeg's mov/mp4 demuxer instead of the native hls_ts path. The
// demuxer sees one continuous logical stream per segment:
//
//	[init fragment (EXT-X-MAP)] [probe prefix] [segment fh]
//
// spliced by fmp4Reader through libav's custom-AVIO bridge
// (FALibavReopen → FALibavOpenFormat, FaLibavOpenStrategyVideoNonSeekable).
// FFmpeg packets are wrapped directly into AVPacket-backed MediaBuf via
// MediaBufFromAVPkt and share the TS path's timestamp/discontinuity
// bookkeeping through enqueuePacketCommon.
//
// Ownership model: a fresh AVFormatContext is opened per segment and
// closed on segment end (FALibavCloseFormat tears down the AVIOContext
// and invokes fmp4Reader.Close — the segment fa_handle itself stays
// owned by hlsSegmentClose). MediaCodecs and the downloaded init
// fragment are cached on fmp4State across segments of the same variant:
// recreating a hardware codec per ~2s fragment would thrash codec slots.
// Each codec owns a private copy of the codecpar (CodecCtxFromStream is
// caller-owned; MediaCodecCreate stores it as mc.FmtCtx and frees it in
// mediaCodecDtor when mc.Format is nil), so caching is lifetime-safe.
package hls

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/czz/movian-go/internal/drm/hlskey"
	"github.com/czz/movian-go/internal/drm/pipe"
	fileaccesscore "github.com/czz/movian-go/internal/fileaccess"
	"github.com/czz/movian-go/internal/libav"
	mediacore "github.com/czz/movian-go/internal/media/core"
	medialibav "github.com/czz/movian-go/internal/media/libav"
	"github.com/czz/movian-go/internal/trace"
)

// fmp4Reader — libav.FileAccessReader splicing the init fragment, the
// already-consumed probe bytes and the live segment handle into one
// sequential, non-seekable stream.
type fmp4Reader struct {
	Init   []byte                 // EXT-X-MAP fragment (may be nil)
	Prefix []byte                 // probe bytes already read from FH
	FH     *fileaccesscore.Handle // segment handle — NOT owned here
	DLC    *int64                 // hd.DownloadCounter
	pos    int
	closed bool
}

var _ libav.FileAccessReader = (*fmp4Reader)(nil)

func (r *fmp4Reader) Read(b []byte) (int, error) {
	if r.closed || len(b) == 0 {
		return 0, io.EOF
	}
	switch {
	case r.pos < len(r.Init):
		n := copy(b, r.Init[r.pos:])
		r.pos += n
		return n, nil
	case r.pos < len(r.Init)+len(r.Prefix):
		n := copy(b, r.Prefix[r.pos-len(r.Init):])
		r.pos += n
		return n, nil
	case r.FH != nil:
		n, err := fileaccesscore.FARead(r.FH, b)
		if n > 0 {
			r.pos += n
			if r.DLC != nil {
				*r.DLC += int64(n)
			}
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	default:
		return 0, io.EOF
	}
}

func (r *fmp4Reader) Seek(off int64, whence int) (int64, error) {
	return -1, errors.New("fmp4Reader: not seekable")
}

// Close is invoked from faLibavCloseAVIO inside FALibavCloseFormat. It
// only seals the splice — the wrapped segment handle belongs to the
// hlsSegment lifecycle.
func (r *fmp4Reader) Close() error {
	r.closed = true
	return nil
}

func (r *fmp4Reader) Size() int64 { return -1 }

// fmp4Stream — per-avformat-stream emit state.
type fmp4Stream struct {
	DataType       int // mediacore.MBVideo / MBAudio
	Stream         int // media queue stream id
	TBNum, TBDen   int // stream time_base
	Codec          *mediacore.MediaCodec
	LoggedKeyframe bool
}

// fmp4CodecEntry — codec cache record; the extradata is kept so a track
// redefinition inside a new init fragment forces codec recreation.
type fmp4CodecEntry struct {
	Codec   *mediacore.MediaCodec
	CodecID int
	Extra   []byte
}

// fmp4State — td.FP. Persists across segments of the same variant.
type fmp4State struct {
	MapKey  string // identity of the loaded init fragment
	Init    []byte
	Fctx    *libav.AVFormatContext  // owning — per segment
	FctxH   *medialibav.AVFormatCtx // non-owning view of Fctx
	Streams map[int]*fmp4Stream     // avformat stream_index → state
	Codecs  map[int]*fmp4CodecEntry // stream_index → codec cache

	// SAMPLE-AES-CTR + KEYFORMAT=identity: shared decryptor seeded
	// with the EXT-X-KEY raw key; attached to every codec as mc.DRM
	// so DRMDecryptPacket (decode path) unwraps cbcs/cenc samples.
	DRM      *pipe.Decryptor
	DRMKeyID string // hv.KeyURL the session was built from
}

// fmp4Probe — the buffered prefix starts with an ISO-BMFF box signature
// ('ftyp' for a self-contained/init file, 'styp' for a media fragment).
func fmp4Probe(buf []byte) bool {
	if len(buf) < 8 {
		return false
	}
	// CMAF segments start with 'styp' and self-contained/init files
	// with 'ftyp'; some encoders lead with 'sidx' or go straight to
	// 'moof' — all are ISO-BMFF fragment signatures.
	switch string(buf[4:8]) {
	case "ftyp", "styp", "sidx", "moof":
		return true
	}
	return false
}

// fmp4LoadInit downloads the EXT-X-MAP init fragment (whole resource or
// its BYTERANGE slice) through fileaccess.
func fmp4LoadInit(hd *hlsDemuxer, hs *hlsSegment) ([]byte, error) {
	foe := &fileaccesscore.OpenExtra{
		OpenTimeout: 3000,
		Cancellable: hd.Cancellable,
	}
	fh, err := fileaccesscore.FAOpenEx(hd.HLS.MP.FAM, hs.MapURL,
		fileaccesscore.FaBufferedBig, foe)
	if fh == nil {
		return nil, err
	}
	if hs.MapOffset >= 0 && hs.MapSize > 0 {
		fh = fileaccesscore.FASliceOpen(fh, int64(hs.MapOffset),
			int64(hs.MapSize))
	}
	defer fh.Close()

	const maxInit = 16 << 20 // sanity cap — init fragments are ~KB
	var out []byte
	buf := make([]byte, 16384)
	for {
		n, rerr := fileaccesscore.FARead(fh, buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			if len(out) > maxInit {
				return nil, fmt.Errorf("init fragment too large")
			}
		}
		if n == 0 || (rerr != nil && n == 0) {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return nil, rerr
			}
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty init fragment")
	}
	return out, nil
}

// tdFmp4Open opens the FFmpeg mov demuxer on [init][segment]. prefix
// holds the probe bytes already consumed from hs.FH — they are the
// start of the media fragment and must be replayed in-band.
func tdFmp4Open(td *tsDemuxer, hv *hlsVariant, hs *hlsSegment,
	prefix []byte) hlsError {
	hd := td.HD
	h := hd.HLS
	sys := libav.GetGlobalLibAVSystem()
	if sys == nil {
		h.ts.Trace(trace.TRACE_ERROR, "HLS",
			"fMP4 segment but libav system not initialised")
		return hlsErrorVariantProbeError
	}

	// Drop any leftover context — segment switches abort the previous
	// open without an EOF pass through tdFmp4ReadPacket.
	tdFmp4CloseSegment(td)

	fp := td.FP
	if fp == nil {
		fp = &fmp4State{Codecs: map[int]*fmp4CodecEntry{}}
		td.FP = fp
	}

	// SAMPLE-AES-CTR + identity key: seed the shared decryptor with
	// the raw EXT-X-KEY payload fetched by hlsSegmentOpen.
	if hs.Crypto == HLSCryptoSampleAESCTR {
		if err := fp.ensureDRM(hd, hv); err != nil {
			h.ts.Trace(trace.TRACE_ERROR, "HLS",
				"SAMPLE-AES-CTR key unusable on segment %d -- %v",
				hs.Seq, err)
			return hlsErrorSegmentBadKey
		}
	}

	// Init fragment — cached by (uri, range) across segments.
	key := fmt.Sprintf("%s@%d:%d", hs.MapURL, hs.MapOffset, hs.MapSize)
	if fp.MapKey != key {
		var init []byte
		if hs.MapURL != "" {
			b, err := fmp4LoadInit(hd, hs)
			if err != nil {
				h.ts.Trace(trace.TRACE_ERROR, "HLS",
					"Unable to load init segment %s -- %v",
					hs.MapURL, err)
				return hlsErrorSegmentBroken
			}
			init = b
		}
		fp.dropCodecs()
		fp.Init = init
		fp.MapKey = key
	}

	reader := &fmp4Reader{
		Init:   fp.Init,
		Prefix: append([]byte(nil), prefix...),
		FH:     hs.FH,
		DLC:    &hd.DownloadCounter,
	}
	avio, err := libav.FALibavReopen(sys, reader, true)
	if avio == nil {
		h.ts.Trace(trace.TRACE_ERROR, "HLS", "FALibavReopen: %v", err)
		return hlsErrorVariantProbeError
	}

	fctx, err := libav.FALibavOpenFormat(avio, "hls-segment.mp4",
		"video/mp4", libav.FaLibavOpenStrategyVideoNonSeekable)
	if fctx == nil {
		h.ts.Trace(trace.TRACE_ERROR, "HLS",
			"Unable to open fMP4 segment %d -- %v", hs.Seq, err)
		libav.FALibavClose(sys, avio)
		return hlsErrorVariantProbeError
	}
	fp.Fctx = fctx
	fp.FctxH = medialibav.WrapFormatCtx(fctx)
	fp.Streams = map[int]*fmp4Stream{}

	muxid := ""
	if hd != &h.Primary {
		muxid = hd.Current.URL
	}

	for i := 0; i < fp.FctxH.GetNumStreams(); i++ {
		si := fp.FctxH.GetStreamInfo(i)
		if si == nil {
			continue
		}
		var dt, stream int
		switch si.CodecType {
		case libav.AvmediaTypeVideo:
			dt = int(mediacore.MBVideo)
			stream = 0
		case libav.AvmediaTypeAudio:
			dt = int(mediacore.MBAudio)
			stream = hlsGetAudioTrack(h, i, muxid,
				fp.FctxH.GetStreamMetadata(i, "language"),
				medialibav.CodecName(si.CodecID), 1)
			if stream == -1 {
				continue
			}
		default:
			continue
		}
		codec := tdFmp4Codec(fp, td, i, si)
		if codec == nil {
			continue
		}
		fp.Streams[i] = &fmp4Stream{
			DataType: dt,
			Stream:   stream,
			TBNum:    si.TimeBaseNum,
			TBDen:    si.TimeBaseDen,
			Codec:    codec,
		}
	}

	if len(fp.Streams) == 0 {
		tdFmp4CloseSegment(td)
		h.ts.Trace(trace.TRACE_ERROR, "HLS",
			"fMP4 segment %d has no usable streams", hs.Seq)
		return hlsErrorVariantProbeError
	}
	hlsTrace(h, "Variant %s segment %d is fMP4 (%d streams)",
		hv.Name, hs.Seq, len(fp.Streams))
	return hlsErrorOK
}

// tdFmp4Codec — create or reuse the MediaCodec for avformat stream i.
// The codec owns a private codecpar copy (mc.FmtCtx, freed by the codec
// dtor because mc.Format is nil), so the cached codec survives the
// per-segment AVFormatContext teardown.
func tdFmp4Codec(fp *fmp4State, td *tsDemuxer, i int,
	si *medialibav.StreamInfo) *mediacore.MediaCodec {
	extra := fp.FctxH.GetStreamExtradata(i)
	if e := fp.Codecs[i]; e != nil {
		if e.CodecID == si.CodecID && bytes.Equal(e.Extra, extra) {
			return e.Codec
		}
		mediacore.MediaCodecDeref(e.Codec)
		delete(fp.Codecs, i)
	}

	cc := fp.FctxH.CodecCtxFromStream(i)
	if cc == nil {
		return nil
	}
	mcp := &mediacore.MediaCodecParams{ExtraData: extra}
	if si.CodecType == libav.AvmediaTypeVideo {
		mcp.Width = si.Width
		mcp.Height = si.Height
		mcp.SARNum, mcp.SARDen = fp.FctxH.GetStreamSampleAspectRatio(i)
	} else {
		mcp.SampleRate = si.SampleRate
		mcp.Channels = si.Channels
	}
	c := mediacore.MediaCodecCreate(mediacore.CodecID(si.CodecID), 0, nil,
		cc.CPtr(), mcp, td.MP)
	if c == nil {
		medialibav.FreeCodecCtx(cc)
		return nil
	}
	c.DRM = fp.DRM
	fp.Codecs[i] = &fmp4CodecEntry{
		Codec: c, CodecID: si.CodecID, Extra: extra,
	}
	return c
}

// ensureDRM builds the shared pipe.Decryptor from the EXT-X-KEY
// identity key in hv.Key, and attaches it to every cached codec.
// Rebuilt when the key URL changes (key rotation).
func (fp *fmp4State) ensureDRM(hd *hlsDemuxer, hv *hlsVariant) error {
	if hv.Key == nil || len(hv.Key.Data) != 16 {
		return fmt.Errorf("identity key is %d bytes, want 16",
			len(hv.Key.Data))
	}
	if fp.DRM != nil && fp.DRMKeyID == hv.KeyURL {
		return nil
	}
	if fp.DRM != nil {
		fp.DRM.Close()
		fp.DRM = nil
	}
	sess, err := hlskey.NewSession(hv.Key.Data)
	if err != nil {
		return err
	}
	d := pipe.New()
	d.AttachSession(hlskey.SystemID, sess)
	fp.DRM = d
	fp.DRMKeyID = hv.KeyURL
	for _, e := range fp.Codecs {
		e.Codec.DRM = d
	}
	return nil
}

// fmp4Rescale — stream time_base → microseconds (AV_TIME_BASE_Q).
func fmp4Rescale(ts int64, num, den int) int64 {
	if ts == libav.AVNoPTSValue || den == 0 {
		return mediacore.PTSUnset
	}
	return libav.AvRescaleQ(ts, libav.AVRational{Num: num, Den: den},
		avTimeBaseQ)
}

// tdFmp4ReadPacket — one av_read_frame round: wraps the AVPacket into a
// MediaBuf and queues it on td.Packets.
//
//	0 = packet queued (or skipped), 1 = segment EOF, else hlsError.
func tdFmp4ReadPacket(td *tsDemuxer, hs *hlsSegment) int {
	hd := td.HD
	fp := td.FP
	pkt, err := fp.FctxH.ReadPacketRaw()
	if pkt == nil {
		tdFmp4CloseSegment(td)
		if err == nil {
			return 1 // EOF/EIO — segment consumed
		}
		return int(hlsErrorSegmentBroken)
	}
	pts, dts, dur, si, _, _ := libav.AvPacketFieldsPtr(pkt.CPtr())
	st := fp.Streams[si]
	if st == nil || st.Codec == nil {
		libav.AvPacketFree(pkt)
		return 0
	}

	keyframe := 0
	if medialibav.PacketFlags(pkt)&libav.AVPktFlagKey != 0 {
		keyframe = 1
	}
	dtsUs := fmp4Rescale(dts, st.TBNum, st.TBDen)
	ptsUs := fmp4Rescale(pts, st.TBNum, st.TBDen)
	mb := mediacore.MediaBufFromAVPkt(td.MP, pkt)
	libav.AvPacketFree(pkt) // mb holds its own ref
	if mb == nil {
		return 0
	}
	mb.Duration = fmp4Rescale(dur, st.TBNum, st.TBDen)

	enqueuePacketCommon(td, mb, nil, dtsUs, ptsUs,
		keyframe, hs, hs.Seq, st.Codec, st.DataType, st.Stream,
		true, dtsUs, ptsUs, &st.LoggedKeyframe, hd)
	return 0
}

// tdFmp4CloseSegment tears down the per-segment AVFormatContext (which
// also closes the AVIO context and seals the fmp4Reader). Codecs and the
// init fragment stay cached.
func tdFmp4CloseSegment(td *tsDemuxer) {
	fp := td.FP
	if fp == nil || fp.Fctx == nil {
		return
	}
	libav.FALibavCloseFormat(libav.GetGlobalLibAVSystem(), fp.Fctx, false)
	fp.Fctx = nil
	fp.FctxH = nil
	fp.Streams = nil
}

func (fp *fmp4State) dropCodecs() {
	for i, e := range fp.Codecs {
		mediacore.MediaCodecDeref(e.Codec)
		delete(fp.Codecs, i)
	}
}

// tdFmp4Close — full teardown from td.destroy / hlsTsDemuxerFlush.
func tdFmp4Close(td *tsDemuxer) {
	tdFmp4CloseSegment(td)
	if fp := td.FP; fp != nil {
		if fp.DRM != nil {
			fp.DRM.Close()
			fp.DRM = nil
		}
		fp.dropCodecs()
		fp.Init = nil
		fp.MapKey = ""
		td.FP = nil
	}
}
