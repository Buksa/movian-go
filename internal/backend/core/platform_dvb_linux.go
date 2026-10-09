//go:build linux && !android

package core

// Linux DVB backend — "dvb:" browses the scanned channel list and
// "dvb:scan" retunes the adapter plan; "dvbts://" is the FA protocol
// that streams one service's TS from the DVR device. New in Go —
// upstream C delegates TV to Tvheadend via HTSP.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/czz/movian-go/internal/dvb"
	fileaccesscore "github.com/czz/movian-go/internal/fileaccess"
	"github.com/czz/movian-go/internal/nls"
	propcore "github.com/czz/movian-go/internal/prop"
	"github.com/czz/movian-go/internal/service"
)

// registerDVBLinux — called from registerPlatformBackends on linux.
// Registers the "dvb:" backend, the "dvbts" FA protocol and the home
// service. No-op when no adapters exist.
func (bs *BackendSystem) registerDVBLinux() {
	if dvb.AdapterCount() == 0 {
		return
	}
	if bs.dvb == nil {
		bs.dvb = &dvbService{bs: bs, store: &dvbStore{}}
		bs.dvb.loadChannels()
	}

	// FA protocol — dvbts://<adapter>/<onid>.<tsid>.<sid> → live TS.
	if fam := bs.FileAccessManager(); fam != nil {
		fam.RegisterProtocol(&dvbFAProtocol{svc: bs.dvb, fam: fam})
	}

	// "dvb:" navigation backend.
	b := &Backend{Prefix: "dvb:"}
	b.CanHandle = func(url string) int {
		if strings.HasPrefix(url, "dvb:") || strings.HasPrefix(url, "dvbts:") {
			return 1
		}
		return 0
	}
	b.Open = func(page any, url0 string, sync bool) error {
		return bs.dvbOpen(page, url0, sync)
	}
	bs.Register(b)

	// Home service — same icon/type as the Android TIF one so the UI
	// presents a single "Live TV" entry regardless of transport.
	if ss := bs.GetServiceSystem(); ss != nil {
		ss.ServiceCreatep("dvb",
			"Live TV", "dvb:", "tv",
			"skin://icons/ic_tv_48px.svg", false, true,
			service.SvcOriginSystem)
	}
}

// ---- backend -----------------------------------------------------------------

// dvbOpen handles "dvb:" (channel page), "dvb:scan" (rescan) and
// "dvbts://" (play a service through the normal video path).
func (bs *BackendSystem) dvbOpen(page any, url string, sync bool) error {
	propRoot, ok := page.(*propcore.Prop)
	if !ok {
		return fmt.Errorf("dvb: non-prop page")
	}

	if strings.HasPrefix(url, "dvbts://") {
		// Playable service URL — hand to the video pipeline.
		bs.Usage().PageOpen(sync, "DVB Channel")
		return bs.OpenVideo(page, url, sync)
	}

	if url == "dvb:scan" {
		bs.dvb.startScan()
		// Fall through to render the channel page — it fills in as
		// services are found on the next open.
	}

	return bs.dvbChannelsPage(propRoot)
}

// dvbChannelsPage renders the stored channel list + a "Scan" action.
func (bs *BackendSystem) dvbChannelsPage(propRoot *propcore.Prop) error {
	pm := bs.propManager
	model := pm.CreateEx(propRoot, "model", nil, false, false)
	pm.SetStringEx(pm.CreateEx(model, "type", nil, false, false),
		nil, "directory", propcore.StringUTF8)
	meta := pm.CreateEx(model, "metadata", nil, false, false)
	pm.SetStringEx(pm.CreateEx(meta, "title", nil, false, false),
		nil, "Live TV", propcore.StringUTF8)
	nodes := pm.CreateEx(model, "nodes", nil, false, false)
	pm.SetIntEx(pm.CreateEx(model, "loading", nil, false, false), nil, 0)

	svc := bs.dvb
	svc.mu.Lock()
	channels := append([]*dvb.Channel(nil), svc.store.Channels...)
	svc.mu.Unlock()

	if len(channels) == 0 && !svc.scanning.Load() {
		// Empty list — offer the scan action.
		bs.dvbAddActionItem(nodes, "Scan channels", "dvb:scan")
	}

	// LCN order where the NIT provided one; unnumbered tail keeps
	// scan order.
	sort.SliceStable(channels, func(a, b int) bool {
		la, lb := channels[a].LCN, channels[b].LCN
		if la > 0 && lb > 0 {
			return la < lb
		}
		return la > 0
	})

	now := time.Now()
	for _, ch := range channels {
		if ch.VideoPID == 0 {
			continue // skip data/radio-only services
		}
		item := pm.CreateEx(nil, "", nil, false, false)
		// "tvchannel" renders items/list/tvchannel.view — title +
		// now/next EPG rows straight off the broadcast EIT.
		pm.SetStringEx(pm.CreateEx(item, "type", nil, false, false),
			nil, "tvchannel", propcore.StringUTF8)
		m := pm.CreateEx(item, "metadata", nil, false, false)
		title := ch.Name
		if title == "" {
			title = fmt.Sprintf("SID %d", ch.SID)
		}
		if ch.LCN > 0 {
			title = fmt.Sprintf("%d  %s", ch.LCN, title)
		}
		pm.SetStringEx(pm.CreateEx(m, "title", nil, false, false),
			nil, title, propcore.StringUTF8)
		url := fmt.Sprintf("dvbts://%d/%04x.%04x.%04x",
			ch.Adapter, ch.ONID, ch.TSID, ch.SID)
		pm.SetStringEx(pm.CreateEx(item, "url", nil, false, false),
			nil, url, propcore.StringUTF8)

		// Events collected from EIT while tuned (this mux plus any
		// other-TS tables broadcasters send). Empty before the
		// first tune — the rows hide themselves.
		ev := pm.CreateEx(m, "events", nil, false, false)
		evList := pm.CreateEx(ev, "list", nil, false, false)
		evCur := pm.CreateEx(ev, "current", nil, false, false)
		evNext := pm.CreateEx(ev, "next", nil, false, false)
		if evs := svc.eitEvents(ch, now.Add(-30*time.Minute).Unix(),
			now.Add(24*time.Hour).Unix()); len(evs) > 0 {
			tvSetEvents(pm, evList, evCur, evNext,
				eitToEPG(evs), now)
		}
		pm.SetParentEx(item, nodes, nil, "")
	}
	return nil
}

// dvbAddActionItem appends a clickable action row (type "action") —
// same convention as the android "Setup channels" item.
func (bs *BackendSystem) dvbAddActionItem(nodes *propcore.Prop, title, url string) {
	pm := bs.propManager
	item := pm.CreateEx(nil, "", nil, false, false)
	pm.SetStringEx(pm.CreateEx(item, "type", nil, false, false),
		nil, "action", propcore.StringUTF8)
	m := pm.CreateEx(item, "metadata", nil, false, false)
	pm.Link(nls.GetProp(title),
		pm.CreateEx(m, "title", nil, false, false), nil, false, false)
	pm.SetStringEx(pm.CreateEx(item, "url", nil, false, false),
		nil, url, propcore.StringUTF8)
	pm.SetParentEx(item, nodes, nil, "")
}

// ---- scan --------------------------------------------------------------------

func (s *dvbService) startScan() {
	if !s.scanning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.scanning.Store(false)
		s.scanAll()
	}()
}

func (s *dvbService) scanAll() {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	// Don't retune while a channel is playing — playback holds tuneMu
	// for its whole duration, so TryLock rather than queue behind it.
	if !s.tuneMu.TryLock() {
		return
	}
	defer s.tuneMu.Unlock()

	adapters, err := dvb.Adapters()
	if err != nil || len(adapters) == 0 {
		return
	}
	var found []*dvb.Channel
	for _, ad := range adapters {
		sc, err := dvb.OpenScanner(ad.ID)
		if err != nil {
			continue
		}
		// Pick the plan by delivery system capability. Terrestrial has a
		// fixed frequency plan; S/C lineups are per-operator and need an
		// operator transponder list (not yet supported).
		var plan []dvb.TuneParams
		for _, f := range ad.Frontends {
			for _, ds := range f.DeliverySystems {
				if ds.IsTerrestrial() {
					plan = dvb.TerrestrialPlanIT()
				}
			}
		}
		if len(plan) == 0 {
			sc.Close()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		chans, _ := sc.Scan(ctx, plan, nil)
		cancel()
		sc.Close()
		found = append(found, chans...)
	}
	s.mu.Lock()
	s.store = &dvbStore{Channels: found, Scanned: time.Now().Unix()}
	s.mu.Unlock()
	s.saveChannels()
}

// ---- dvbts: file access protocol ------------------------------------------------

// dvbFAProtocol serves "dvbts://" URLs as live MPEG-TS: tune the
// frontend, set demux PID filters, stream dvr0.
type dvbFAProtocol struct {
	svc *dvbService
	fam *fileaccesscore.FileAccessManager
}

func (p *dvbFAProtocol) Name() string { return "dvbts" }

func (p *dvbFAProtocol) CanHandle(url string) bool {
	return strings.HasPrefix(url, "dvbts://")
}

// Open resolves the channel, tunes and returns a Handle whose reader
// is the filtered TS. fmt: dvbts://<adapter>/<onid>.<tsid>.<sid>
func (p *dvbFAProtocol) Open(url string, extra *fileaccesscore.OpenExtra) (*fileaccesscore.Handle, error) {
	adapter, onid, tsid, sid, err := parseDVBURL(url)
	if err != nil {
		return nil, err
	}
	p.svc.mu.Lock()
	var ch *dvb.Channel
	for _, c := range p.svc.store.Channels {
		if c.Adapter == adapter && c.ONID == onid && c.TSID == tsid && c.SID == sid {
			ch = c
			break
		}
	}
	p.svc.mu.Unlock()
	if ch == nil {
		return nil, fmt.Errorf("dvb: unknown channel %04x.%04x.%04x", onid, tsid, sid)
	}
	if ch.FreeCA {
		return nil, fmt.Errorf("dvb: %s is scrambled (CA)", ch.Name)
	}
	if ch.Transport == nil {
		return nil, fmt.Errorf("dvb: channel %s has no tuning data", ch.Name)
	}
	if p.svc.scanning.Load() {
		return nil, fmt.Errorf("dvb: channel scan in progress")
	}

	// One tune at a time — the frontend is a single resource. The tune
	// mutex is released by dvbReader.Close.
	p.svc.tuneMu.Lock()

	fe, err := dvb.OpenFrontend(adapter, 0)
	if err != nil {
		p.svc.tuneMu.Unlock()
		return nil, err
	}
	fail := func(e error) (*fileaccesscore.Handle, error) {
		fe.Close()
		p.svc.tuneMu.Unlock()
		return nil, e
	}
	if err := fe.Tune(ch.Transport); err != nil {
		return fail(fmt.Errorf("dvb: tune: %w", err))
	}
	if ok, err := fe.WaitLock(1500 * time.Millisecond); err != nil || !ok {
		return fail(fmt.Errorf("dvb: no lock on %s", ch.Name))
	}

	// PID filters — PAT + PMT + PCR + elementary streams.
	var dmxs []*dvb.Demux
	openFilter := func(pid uint16) error {
		dm, err := dvb.OpenDemux(adapter, 0)
		if err != nil {
			return err
		}
		if err := dm.SetPESFilter(pid); err != nil {
			dm.Close()
			return err
		}
		dmxs = append(dmxs, dm)
		return nil
	}
	pids := []uint16{dvb.PIDPAT, ch.PMTPID, ch.PCRPID, ch.VideoPID}
	pids = append(pids, ch.AudioPIDs...)
	pids = append(pids, ch.SubPIDs...)
	seen := map[uint16]bool{}
	for _, pid := range pids {
		if pid == 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		if err := openFilter(pid); err != nil {
			for _, dm := range dmxs {
				dm.Close()
			}
			return fail(fmt.Errorf("dvb: filter pid %d: %w", pid, err))
		}
	}
	dvr, err := dvb.OpenDVR(adapter, 0)
	if err != nil {
		for _, dm := range dmxs {
			dm.Close()
		}
		return fail(err)
	}

	// Broadcast EPG — a second demux on PID 0x12 delivers complete EIT
	// sections (the kernel reassembles + CRC-checks them) while dvr0
	// serves the elementary streams.
	eitStop := make(chan struct{})
	if dm, err := dvb.OpenDemux(adapter, 0); err == nil {
		if err := dm.SetSectionFilter(dvb.PIDEIT, nil, nil, 0); err == nil {
			go p.svc.eitCollect(dm, eitStop)
		} else {
			dm.Close()
		}
	}

	rdr := &dvbReader{dvr: dvr, dmxs: dmxs, fe: fe, svc: p.svc,
		eitStop: eitStop}
	return fileaccesscore.NewHandle(p.fam, p, url, rdr, nil, nil,
		func() int64 { return -1 }), nil
}

// eitCollect drains EIT sections while the frontend is tuned and
// merges them into the shared guide. Runs until stop closes.
func (s *dvbService) eitCollect(dm *dvb.Demux, stop <-chan struct{}) {
	defer dm.Close()
	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		ok, err := dm.Poll(300 * time.Millisecond)
		if err != nil {
			return
		}
		if !ok {
			continue
		}
		n, err := dm.ReadSection(buf)
		if err != nil || n <= 0 {
			continue
		}
		sec, err := dvb.ParseSection(buf[:n])
		if err != nil || !dvb.IsEIT(sec.TableID) || !sec.Current {
			continue
		}
		if evs, err := dvb.ParseEIT(sec); err == nil {
			s.eitMerge(evs)
		}
	}
}

// dvbReader — io.ReadCloser bridging the DVR ring buffer. Read blocks
// via Poll; Close releases the demux filters, frontend and tune mutex.
type dvbReader struct {
	dvr     *dvb.DVR
	dmxs    []*dvb.Demux
	fe      *dvb.Frontend
	svc     *dvbService
	eitStop chan struct{}
	done    bool
}

func (r *dvbReader) Read(b []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	for {
		ok, err := r.dvr.Poll(500 * time.Millisecond)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue // keep waiting — live TV never ends
		}
		n, err := r.dvr.Read(b)
		if n > 0 {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func (r *dvbReader) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	close(r.eitStop) // releases the EIT collector goroutine
	for _, dm := range r.dmxs {
		dm.Close()
	}
	r.dvr.Close()
	r.fe.Close()
	r.svc.tuneMu.Unlock()
	return nil
}

// Stat — live stream: no size, content type file.
func (p *dvbFAProtocol) Stat(url string) (*fileaccesscore.FileStat, error) {
	return &fileaccesscore.FileStat{Size: -1, Type: fileaccesscore.ContentFile}, nil
}

// ScanDir — dvbts URLs are leaf streams; no directory listing.
func (p *dvbFAProtocol) ScanDir(ctx context.Context, url string) (*fileaccesscore.Dir, error) {
	return nil, fmt.Errorf("dvbts: not a directory")
}

func parseDVBURL(url string) (adapter int, onid, tsid, sid uint16, err error) {
	rest := strings.TrimPrefix(url, "dvbts://")
	host, path, ok := strings.Cut(rest, "/")
	bad := func() error { return fmt.Errorf("dvb: bad url %q", url) }
	if !ok {
		return 0, 0, 0, 0, bad()
	}
	a, e := strconv.Atoi(host)
	if e != nil || a < 0 {
		return 0, 0, 0, 0, bad()
	}
	parts := strings.Split(path, ".")
	if len(parts) != 3 {
		return 0, 0, 0, 0, bad()
	}
	ids := make([]uint16, 3)
	for i, p := range parts {
		v, e := strconv.ParseUint(p, 16, 16)
		if e != nil {
			return 0, 0, 0, 0, bad()
		}
		ids[i] = uint16(v)
	}
	return a, ids[0], ids[1], ids[2], nil
}
