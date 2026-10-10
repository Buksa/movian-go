//go:build android

package core

// Platform backends: android-only extras registered in Start().

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/czz/movian-go/internal/arch"
	propcore "github.com/czz/movian-go/internal/prop"
	"github.com/czz/movian-go/internal/trace"
)

// appsEntry — one launchable Android app from Apps.list() (JSON).
type appsEntry struct {
	Pkg   string `json:"pkg"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// registerPlatformBackends — android registers the "apps:" launcher
// backend and the "androidsettings:" action backend (opens the system
// Settings app, then self-closes). New in Go — upstream C has no
// app-launcher feature.
func (bs *BackendSystem) registerPlatformBackends() {
	settingsBackend := &Backend{Prefix: "androidsettings:"}
	settingsBackend.CanHandle = func(url string) int {
		if strings.HasPrefix(url, "androidsettings:") {
			return 1
		}
		return 0
	}
	settingsBackend.Open = func(page any, url0 string, sync bool) error {
		bs.traceSystem.Trace(trace.TRACE_DEBUG, "APPS",
			"openurl %s -> openSettings", url0)
		arch.AndroidOpenSettings()
		// Close this page — Android Settings is now in the foreground
		// and there is nothing to show here when the user returns.
		if propRoot, ok := page.(*propcore.Prop); ok && propRoot != nil {
			bs.propManager.SetIntEx(
				bs.propManager.CreateEx(propRoot, "close", nil, false, false),
				nil, 1)
		}
		return nil
	}
	bs.Register(settingsBackend)

	backend := &Backend{Prefix: "apps:"}
	backend.CanHandle = func(url string) int {
		if strings.HasPrefix(url, "apps:") {
			return 1
		}
		return 0
	}
	backend.Open = func(page any, url0 string, sync bool) error {
		propRoot, ok := page.(*propcore.Prop)
		if !ok {
			return fmt.Errorf("apps: backend cannot handle non-prop page")
		}

		pm := bs.propManager
		model := pm.CreateEx(propRoot, "model", nil, false, false)
		pm.SetStringEx(pm.CreateEx(model, "type", nil, false, false),
			nil, "directory", propcore.StringUTF8)
		meta := pm.CreateEx(model, "metadata", nil, false, false)
		pm.SetStringEx(pm.CreateEx(meta, "title", nil, false, false),
			nil, "Applications", propcore.StringUTF8)
		nodes := pm.CreateEx(model, "nodes", nil, false, false)

		// Convention: backends own model.loading (fa_filepicker,
		// openErrorf etc. do the same — void would also hide the
		// spinner, but explicit 0 matches the C pattern).
		pm.SetIntEx(pm.CreateEx(model, "loading", nil, false, false),
			nil, 0)

		raw := arch.AndroidAppsList()
		var apps []appsEntry
		if err := json.Unmarshal([]byte(raw), &apps); err != nil {
			bs.traceSystem.Trace(trace.TRACE_ERROR, "APPS",
				"list failed: %v raw=%q", err, raw)
			return nil // empty page rather than openerror
		}
		bs.traceSystem.Trace(trace.TRACE_DEBUG, "APPS",
			"list: %d apps (raw %d bytes)", len(apps), len(raw))

		for _, a := range apps {
			// Build the item detached and attach it to nodes last:
			// inserting into nodes fires PROP_ADD_CHILD, which makes the
			// GLW cloner evaluate the item view immediately — eventSink
			// must already exist or deliverEvent($self.eventSink) resolves
			// to nothing and activate stays dead.
			item := pm.CreateEx(nil, "", nil, false, false)
			pm.SetStringEx(pm.CreateEx(item, "type", nil, false, false),
				nil, "androidapp", propcore.StringUTF8)
			im := pm.CreateEx(item, "metadata", nil, false, false)
			pm.SetStringEx(pm.CreateEx(im, "title", nil, false, false),
				nil, a.Label, propcore.StringUTF8)
			if a.Icon != "" {
				pm.SetStringEx(pm.CreateEx(im, "icon", nil, false, false),
					nil, a.Icon, propcore.StringUTF8)
			}

			// action.view: deliverEvent($self.eventSink) on activate —
			// the eventSink subscriber launches the package.
			pkg := a.Pkg
			es := pm.CreateEx(item, "eventSink", nil, false, false)
			es.Subscribe(
				func(o any, et propcore.EventType, args ...any) {
					bs.traceSystem.Trace(trace.TRACE_DEBUG, "APPS",
						"eventSink fired pkg=%s et=%d", pkg, int(et))
					if et == propcore.EventExtEvent {
						arch.AndroidAppLaunch(pkg)
					}
				}, nil, propcore.SubNoInitialUpdate)

			pm.SetParentEx(item, nodes, nil, "")
		}
		return nil
	}
	bs.Register(backend)

	// TIF backend — "tvinput:" URLs: Live TV service page, channel
	// tune/untune, input setup passthrough. Registered only when the
	// device exposes TvInputServices.
	if arch.AndroidTvInputCount() > 0 {
		tvBackend := &Backend{Prefix: "tvinput:"}
		tvBackend.CanHandle = func(url string) int {
			if strings.HasPrefix(url, "tvinput:") {
				return 1
			}
			return 0
		}
		tvBackend.Open = func(page any, url0 string, sync bool) error {
			return bs.tvOpen(page, url0, sync)
		}
		bs.Register(tvBackend)
	}
}
