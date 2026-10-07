//go:build windows

package core

// WASAPI audio driver — NO upstream C counterpart (upstream never
// shipped win32 audio; mac_audio.c's AudioQueue driver is the closest
// model). Shared-mode polled IAudioClient on the default render
// endpoint: the DeliverUnlocked seam maps to GetCurrentPadding +
// IAudioRenderClient GetBuffer/ReleaseBuffer, the clock anchor maps to
// IAudioClock::GetPosition, volume to ISimpleAudioVolume.

/*
#cgo windows LDFLAGS: -lole32 -luuid -lksuser -lavrt
#define COBJMACROS
#define CINTERFACE
#include <windows.h>
#include <mmdeviceapi.h>
#include <audioclient.h>
#include <avrt.h>
#include <functiondiscoverykeys_devpkey.h>
#include <propidl.h>
#include <initguid.h>
#include <string.h>
#include <stdint.h>

// The WASAPI GUIDs are not in mingw's libuuid — define them locally
// (values from the SDK headers).
DEFINE_GUID(CLSID_MMDeviceEnumerator, 0xBCDE0395, 0xE52F, 0x467C,
            0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E);
DEFINE_GUID(IID_IMMDeviceEnumerator, 0xA95664D2, 0x9614, 0x4F35,
            0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6);
DEFINE_GUID(IID_IAudioClient, 0x1CB9AD4C, 0xDBFA, 0x4c32,
            0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2);
DEFINE_GUID(IID_IAudioRenderClient, 0xF294ACFC, 0x3146, 0x4483,
            0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2);
DEFINE_GUID(IID_ISimpleAudioVolume, 0x87FB5498, 0x68A6, 0x4E40,
            0x92, 0x15, 0x01, 0x47, 0xA5, 0xA1, 0x34, 0xDC);
DEFINE_GUID(IID_IAudioClock, 0xCD63314F, 0x3FBA, 0x4a1b,
            0x81, 0x2C, 0xEF, 0x96, 0x35, 0x87, 0x28, 0xE7);
// PKEY_Device_FriendlyName = {a45c254e-df1c-4efd-8020-67d146a850e0}, 14
// (mingw's functiondiscoverykeys doesn't export the symbol)
static const PROPERTYKEY ml_PKEY_Device_FriendlyName = {
    {0xa45c254e, 0xdf1c, 0x4efd,
     {0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0}}, 14};

typedef struct {
    IAudioClient        *client;
    IAudioRenderClient  *render;
    ISimpleAudioVolume  *vol;
    IAudioClock         *clock;
    IMMDevice           *dev;
    uint32_t             buf_frames;
    uint32_t             frame_bytes;
    uint32_t             rate;
} ml_wasapi;

// ml_wasapi_open — CoCreateInstance(MMDeviceEnumerator) →
// GetDefaultAudioEndpoint(eRender) → Activate(IAudioClient) →
// Initialize(shared, polled, ~40ms) → GetService(render/vol/clock).
// fmt: float32 PCM at the engine mix rate (caller's resampler —
// ad.AVR — converts).
static int
ml_wasapi_open(ml_wasapi *w, uint32_t rate, uint16_t channels)
{
    IMMDeviceEnumerator *en = NULL;
    IMMDevice *dev = NULL;
    WAVEFORMATEX *mix = NULL, wfx;
    REFERENCE_TIME dur = 400000; // 40ms in 100ns units
    HRESULT hr;

    memset(w, 0, sizeof(*w));

    CoInitializeEx(NULL, COINIT_MULTITHREADED);

    hr = CoCreateInstance(&CLSID_MMDeviceEnumerator, NULL,
        CLSCTX_ALL, &IID_IMMDeviceEnumerator, (void **)&en);
    if(FAILED(hr))
        return -2;
    hr = en->lpVtbl->GetDefaultAudioEndpoint(en, eRender,
        eMultimedia, &dev);
    en->lpVtbl->Release(en);
    if(FAILED(hr))
        return -3;
    hr = dev->lpVtbl->Activate(dev, &IID_IAudioClient, CLSCTX_ALL,
        NULL, (void **)&w->client);
    if(FAILED(hr)) {
        dev->lpVtbl->Release(dev);
        return -4;
    }
    w->dev = dev;

    hr = w->client->lpVtbl->GetMixFormat(w->client, &mix);
    if(FAILED(hr))
        return -5;
    rate = mix->nSamplesPerSec; // shared mode: engine rate wins
    w->rate = rate;
    CoTaskMemFree(mix);

    memset(&wfx, 0, sizeof(wfx));
    wfx.wFormatTag = WAVE_FORMAT_IEEE_FLOAT;
    wfx.nChannels = channels;
    wfx.nSamplesPerSec = rate;
    wfx.wBitsPerSample = 32;
    wfx.nBlockAlign = wfx.nChannels * 4;
    wfx.nAvgBytesPerSec = rate * wfx.nBlockAlign;

    // Shared mode, polled (no EVENTCALLBACK): old drivers (e.g. IDT on
    // pre-Win10-era laptops) wedge the render stream in event mode —
    // GetCurrentPadding sticks at buf_frames after the first fill and
    // the event never refires. Poll padding with a short sleep instead;
    // the caller adds an IAudioClock position cross-check.
    hr = w->client->lpVtbl->Initialize(w->client,
        AUDCLNT_SHAREMODE_SHARED,
        0, dur, 0, &wfx, NULL);
    if(FAILED(hr))
        return -6;
    w->client->lpVtbl->GetBufferSize(w->client, &w->buf_frames);
    w->frame_bytes = wfx.nBlockAlign;

    hr = w->client->lpVtbl->GetService(w->client,
        &IID_IAudioRenderClient, (void **)&w->render);
    if(FAILED(hr))
        return -7;
    w->client->lpVtbl->GetService(w->client,
        &IID_ISimpleAudioVolume, (void **)&w->vol);
    w->client->lpVtbl->GetService(w->client,
        &IID_IAudioClock, (void **)&w->clock);
    return 0;
}

// ml_wasapi_padding — frames currently queued (GetCurrentPadding).
static uint32_t
ml_wasapi_padding(ml_wasapi *w)
{
    uint32_t p = 0;
    if(w->client)
        w->client->lpVtbl->GetCurrentPadding(w->client, &p);
    return p;
}

// ml_wasapi_write — write `frames` interleaved frames. The caller has
// already computed free space (GetCurrentPadding, with an
// IAudioClock::GetPosition fallback for drivers where padding wedges at
// buf_frames); re-checking padding here would re-clamp to the bogus 0.
static uint32_t
ml_wasapi_write(ml_wasapi *w, const void *data, uint32_t frames)
{
    BYTE *buf = NULL;

    if(frames == 0)
        return 0;
    if(FAILED(w->render->lpVtbl->GetBuffer(w->render, frames, &buf)))
        return 0;
    memcpy(buf, data, frames * w->frame_bytes);
    w->render->lpVtbl->ReleaseBuffer(w->render, frames, 0);
    return frames;
}

// ml_wasapi_position — IAudioClock::GetPosition: device position in
// frames + the QPC timestamp it corresponds to (100ns units).
static int
ml_wasapi_position(ml_wasapi *w, uint64_t *pos, uint64_t *qpc)
{
    if(w->clock == NULL)
        return -1;
    return FAILED(w->clock->lpVtbl->GetPosition(w->clock, pos, qpc))
        ? -1 : 0;
}

static int ml_wasapi_start(ml_wasapi *w)
{
    if(w->client)
        return FAILED(w->client->lpVtbl->Start(w->client)) ? -1 : 0;
    return -1;
}
static void ml_wasapi_stop(ml_wasapi *w)
{
    if(w->client)
        w->client->lpVtbl->Stop(w->client);
}
static void ml_wasapi_reset(ml_wasapi *w)
{
    if(w->client)
        w->client->lpVtbl->Reset(w->client);
}
static void ml_wasapi_volume(ml_wasapi *w, float level)
{
    if(w->vol)
        w->vol->lpVtbl->SetMasterVolume(w->vol, level, NULL);
}

// ml_wasapi_devname — friendly name of the endpoint.
static int
ml_wasapi_devname(ml_wasapi *w, char *out, int outsz)
{
    IPropertyStore *ps;
    PROPVARIANT v;
    HRESULT hr;
    if(!w->dev)
        return -1;
    hr = w->dev->lpVtbl->OpenPropertyStore(w->dev, STGM_READ, &ps);
    if(FAILED(hr))
        return -1;
    PropVariantInit(&v);
    hr = ps->lpVtbl->GetValue(ps, &ml_PKEY_Device_FriendlyName, &v);
    ps->lpVtbl->Release(ps);
    if(FAILED(hr) || v.vt != VT_LPWSTR) {
        PropVariantClear(&v);
        return -1;
    }
    WideCharToMultiByte(CP_UTF8, 0, v.pwszVal, -1, out, outsz,
        NULL, NULL);
    PropVariantClear(&v);
    return 0;
}

static void
ml_wasapi_close(ml_wasapi *w)
{
    if(w->client) {
        w->client->lpVtbl->Stop(w->client);
        w->client->lpVtbl->Release(w->client);
    }
    if(w->render) w->render->lpVtbl->Release(w->render);
    if(w->vol)    w->vol->lpVtbl->Release(w->vol);
    if(w->clock)  w->clock->lpVtbl->Release(w->clock);
    if(w->dev)    w->dev->lpVtbl->Release(w->dev);

}
*/
import "C"

import (
	"errors"
	"unsafe"

	archpkg "github.com/czz/movian-go/internal/arch"
	mediacore "github.com/czz/movian-go/internal/media/core"
	"github.com/czz/movian-go/internal/trace"
)

// wasapiDecoder — hangs off AudioDecoder.AudioInstance like C's
// ac_alloc_size embedding (mac_audio.c decoder_t role).
type wasapiDecoder struct {
	owner     *AudioDecoder
	w         C.ml_wasapi
	framesize int
	tmp       []byte
	samples   int64 // frames written since (re)start
	underrun  int
}

func (ad *AudioDecoder) getWasapi() *wasapiDecoder {
	if d, ok := ad.AudioInstance.(*wasapiDecoder); ok {
		return d
	}
	d := &wasapiDecoder{owner: ad}
	ad.AudioInstance = d
	return d
}

// wasapiAudioReconfig — mac_audio_reconfig's role: close the old
// client, open on the engine mix rate, start.
func wasapiAudioReconfig(ad *AudioDecoder) int {
	d := ad.getWasapi()

	if d.w.client != nil {
		C.ml_wasapi_close(&d.w)
		d.w.client = nil
	}

	ad.OutChannelLayout = ChannelLayoutStereo
	ad.OutSampleFormat = SampleFormatFLT // WASAPI shared native
	ad.OutSampleRate = ad.InSampleRate   // provisional

	r := C.ml_wasapi_open(&d.w, C.uint32_t(ad.InSampleRate), C.uint16_t(2))
	if r != 0 {
		ad.ts.Trace(trace.TRACE_ERROR, "WASAPI",
			"open failed (%d) — audio disabled", int(r))
		return 1
	}
	var dname [256]C.char
	if C.ml_wasapi_devname(&d.w, &dname[0], 256) == 0 {
		ad.ts.Trace(trace.TRACE_DEBUG, "WASAPI",
			"endpoint: %s", C.GoString(&dname[0]))
	}
	// Shared mode pins the engine mix rate — read it back (it may
	// differ from what we asked for; AVR converts).
	ad.OutSampleRate = int(d.w.rate)
	d.framesize = int(d.w.frame_bytes)
	d.tmp = make([]byte, 4096*d.framesize)
	d.samples = 0

	ad.ts.Trace(trace.TRACE_DEBUG, "WASAPI", "Start %d Hz",
		ad.OutSampleRate)
	if C.ml_wasapi_start(&d.w) != 0 {
		ad.ts.Trace(trace.TRACE_ERROR, "WASAPI",
			"IAudioClient::Start failed — stream never enters mixer")
	}
	return 0
}

// wasapiAudioDeliver — alsa_audio_deliver's role: wait for buffer
// space (polled, like snd_pcm_wait), pull from ad.AVR, write, anchor
// the audio clock, report ad.Delay. Returns 0.
func wasapiAudioDeliver(ad *AudioDecoder, samples int, pts int64,
	epoch int) int {
	d := ad.getWasapi()
	mp, _ := ad.MediaPipe.(*mediacore.MediaPipe)
	if d.w.client == nil {
		return -1
	}

	// C: snd_pcm_wait(h, 100) — poll GetCurrentPadding; the engine
	// drains ~10ms device periods.
	pad := int(C.ml_wasapi_padding(&d.w))
	avail := int(d.w.buf_frames) - pad
	if avail <= 0 {
		C.Sleep(10)
		pad = int(C.ml_wasapi_padding(&d.w))
		avail = int(d.w.buf_frames) - pad
		if avail <= 0 {
			// Padding can wedge at buf_frames on some drivers
			// (device keeps playing, padding never drops — e.g.
			// old IDT/Realtek). Cross-check against
			// IAudioClock::GetPosition: frames really played is
			// d.samples - pos; space is what remains.
			var pos, qpc C.uint64_t
			if C.ml_wasapi_position(&d.w, &pos, &qpc) == 0 {
				inflight := d.samples - int64(pos)
				if inflight < 0 {
					inflight = 0
				}
				if a2 := int(d.w.buf_frames) - int(inflight); a2 > avail {
					avail = a2
				}
			}
			if avail <= 0 {
				return 10 // retry soon — engine drains ~10ms periods
			}
		}
	}

	cInt := avail
	if cInt > samples {
		cInt = samples
	}
	if cap(d.tmp) < cInt*d.framesize {
		d.tmp = make([]byte, cInt*d.framesize)
	}
	got := 0
	if ad.AVR != nil && cInt > 0 {
		need := cInt * ad.AVR.bytesPerSample
		if need > len(d.tmp) {
			need = len(d.tmp)
		}
		got = ad.AVR.Read(d.tmp[:need], cInt)
		cInt = got
	}

	// Clock anchor — IAudioClock::GetPosition gives the device play
	// position in frames + the QPC it maps to: the frames being
	// written now will play at avtime ≈ now + delay. Same anchor
	// semantics as alsa's trigger_htstamp + samples.
	if pts != mediacore.PTSUnset && mp != nil {
		ad.Delay = int(1000000 * int64(pad) / int64(ad.OutSampleRate))
		mp.ClockMutex.Lock()
		mp.AudioClockAvtime = archpkg.GetTS() + int64(ad.Delay)
		mp.AudioClock = pts
		mp.AudioClockEpoch = int64(epoch)
		mp.ClockMutex.Unlock()
	}

	if cInt > 0 {
		w := int(C.ml_wasapi_write(&d.w, unsafe.Pointer(&d.tmp[0]),
			C.uint32_t(cInt)))
		d.samples += int64(w)
	}
	return 0
}

func wasapiAudioFini(ad *AudioDecoder) {
	d := ad.getWasapi()
	if d.w.client != nil {
		C.ml_wasapi_close(&d.w)
		d.w.client = nil
	}
}

func wasapiAudioPause(ad *AudioDecoder) {
	d := ad.getWasapi()
	C.ml_wasapi_stop(&d.w)
}

func wasapiAudioPlay(ad *AudioDecoder) {
	d := ad.getWasapi()
	if d.w.client != nil {
		C.ml_wasapi_start(&d.w)
	}
}

func wasapiAudioFlush(ad *AudioDecoder) {
	d := ad.getWasapi()
	if d.w.client != nil {
		C.ml_wasapi_stop(&d.w)
		C.ml_wasapi_reset(&d.w)
		C.ml_wasapi_start(&d.w)
		d.samples = 0
	}
}

func wasapiAudioSetVolume(ad *AudioDecoder, level float32) {
	d := ad.getWasapi()
	C.ml_wasapi_volume(&d.w, C.float(level))
}

// errReconfig — wasapiAudioReconfig's nonzero return (device open or
// stream init failure).
var errReconfig = errors.New("wasapi: reconfig failed")

// audioDriverStartPlatform — the windows driver (wasapi); same
// AudioClass seam as alsa/mac_audio.
func audioDriverStartPlatform(settings any) (*AudioClass, error) {
	return &AudioClass{
		Fini: wasapiAudioFini,
		Reconfig: func(ad *AudioDecoder) error {
			if wasapiAudioReconfig(ad) != 0 {
				return errReconfig
			}
			return nil
		},
		DeliverUnlocked: wasapiAudioDeliver,
		Pause:           wasapiAudioPause,
		Play:            wasapiAudioPlay,
		Flush:           wasapiAudioFlush,
		SetVolume:       wasapiAudioSetVolume,
	}, nil
}
