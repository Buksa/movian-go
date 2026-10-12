package prop

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dop251/goja"
)

type settingsHarness struct {
	vm               *goja.Runtime
	prop             *goja.Object
	settings         *goja.Object
	numericKeySeen   bool
	stringNumericIDs map[string]int
	multiOptValue    string
	selectCalls      int
	linkCalls        int
}

func newSettingsHarness(t *testing.T, rejectNumericChild bool) *settingsHarness {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(testFile), "..", "..")
	propSource, err := os.ReadFile(filepath.Join(root, "res", "ecmascript", "modules", "movian", "prop.js"))
	if err != nil {
		t.Fatalf("read movian/prop module: %v", err)
	}
	settingsSource, err := os.ReadFile(filepath.Join(root, "res", "ecmascript", "modules", "movian", "settings.js"))
	if err != nil {
		t.Fatalf("read movian/settings module: %v", err)
	}

	h := &settingsHarness{
		vm:               goja.New(),
		stringNumericIDs: make(map[string]int),
	}
	np := h.vm.NewObject()
	np.Set("global", func() *goja.Object { return h.vm.NewObject() })
	np.Set("create", func() *goja.Object { return h.vm.NewObject() })
	np.Set("getValue", func(goja.Value) any { return nil })
	np.Set("getChild", func(_ goja.Value, name goja.Value) goja.Value {
		switch name.Export().(type) {
		case int64, int32, int, uint64, uint32, uint, float64:
			h.numericKeySeen = true
			if rejectNumericChild {
				return goja.Undefined()
			}
			return h.vm.NewObject()
		}
		if key, ok := name.Export().(string); ok {
			if key == "0" || key == "1" || key == "2" || key == "3" || key == "4" {
				h.stringNumericIDs[key]++
			}
		}
		return h.vm.NewObject()
	})
	np.Set("set", func(_ goja.Value, name, value goja.Value) {
		if name.String() == "value" {
			h.multiOptValue = value.String()
		}
	})
	np.Set("setRichStr", func(...goja.Value) {})
	np.Set("isValue", func(goja.Value) bool { return false })
	np.Set("enumerate", func(...goja.Value) []string { return nil })
	np.Set("has", func(...goja.Value) bool { return false })
	np.Set("deleteChild", func(...goja.Value) bool { return true })
	np.Set("subscribe", func(...goja.Value) {})
	np.Set("select", func(...goja.Value) { h.selectCalls++ })
	np.Set("link", func(...goja.Value) { h.linkCalls++ })
	np.Set("setClipRange", func(...goja.Value) {})
	np.Set("unloadDestroy", func(...goja.Value) {})
	np.Set("makeUrl", func(...goja.Value) goja.Value { return h.vm.ToValue("settings://test") })

	h.prop = loadTestModule(t, h.vm, propSource, func(name string) *goja.Object {
		if name != "native/prop" {
			t.Fatalf("unexpected prop module request %q", name)
		}
		return np
	})

	kvstore := h.vm.NewObject()
	kvstore.Set("getString", func(args ...goja.Value) goja.Value {
		if len(args) >= 3 && args[2].String() == "genre" {
			return h.vm.ToValue("1")
		}
		if len(args) >= 3 && args[2].String() == "1" {
			return h.vm.ToValue("stored text")
		}
		return goja.Null()
	})
	kvstore.Set("getBoolean", func(...goja.Value) goja.Value { return h.vm.ToValue(true) })
	kvstore.Set("getInteger", func(...goja.Value) goja.Value { return h.vm.ToValue(7) })
	kvstore.Set("set", func(...goja.Value) {})
	store := h.vm.NewObject()
	store.Set("createFromPath", func(...goja.Value) *goja.Object { return h.vm.NewObject() })
	fs := h.vm.NewObject()
	fs.Set("mkdirs", func(...goja.Value) {})
	h.settings = loadTestModule(t, h.vm, settingsSource, func(name string) *goja.Object {
		switch name {
		case "movian/prop":
			return h.prop
		case "movian/store":
			return store
		case "native/kvstore":
			return kvstore
		case "native/fs":
			return fs
		default:
			t.Fatalf("unexpected settings module request %q", name)
			return nil
		}
	})
	h.vm.Set("settings", h.settings)
	h.vm.Set("prop", h.prop)
	h.vm.Set("Core", map[string]any{"storagePath": "/tmp/movian-go-settings-test"})
	return h
}

func (h *settingsHarness) run(t *testing.T, source string) {
	t.Helper()
	if _, err := h.vm.RunString(source); err != nil {
		t.Fatalf("run settings compatibility scenario: %v", err)
	}
}

func TestMovianSettingsMultiOptUsesStringOptionIDs(t *testing.T) {
	h := newSettingsHarness(t, true)
	h.run(t, `(function(settings, prop) {
		var group = new settings.kvstoreSettings(prop.makeProp({}), "page:test", "domain");
		group.createMultiOpt("genre", "Genre", [
			["0", "All", true],
			["1", "Movies"]
		], function() {}, true);
	})(settings, prop)`)
	if h.numericKeySeen {
		t.Fatal("createMultiOpt converted a string option ID to a numeric index")
	}
	if h.stringNumericIDs["0"] == 0 || h.stringNumericIDs["1"] == 0 {
		t.Fatalf("multiopt option IDs were not passed as string children: %v", h.stringNumericIDs)
	}
	if h.multiOptValue != "1" || h.selectCalls != 1 || h.linkCalls != 1 {
		t.Fatalf("persisted multiopt selection = value %q, select calls %d, link calls %d; want 1/1/1",
			h.multiOptValue, h.selectCalls, h.linkCalls)
	}
}

func TestCreateSettingConstructorsUseNamedNumericIDs(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"bool", `group.createBool("0", "Bool", false, function() {}, true);`},
		{"string", `group.createString("1", "String", "", function() {}, true);`},
		{"integer", `group.createInt("2", "Integer", 0, 0, 10, 1, "", function() {}, true);`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSettingsHarness(t, true)
			h.run(t, `(function(settings, prop) {
				var group = new settings.kvstoreSettings(prop.makeProp({}), "page:test", "domain");
				`+tt.source+`
			})(settings, prop)`)
			if h.numericKeySeen {
				t.Fatalf("create%s treated a named numeric ID as an index", tt.name)
			}
			if h.stringNumericIDs["0"]+h.stringNumericIDs["1"]+h.stringNumericIDs["2"] == 0 {
				t.Fatal("no numeric-looking setting ID reached native getChild as a string")
			}
		})
	}
}

func TestGlobalSettingsUsesNamedNumericIDs(t *testing.T) {
	h := newSettingsHarness(t, true)
	h.run(t, `(function(settings, prop) {
		var group = new settings.globalSettings("3", "Title", "icon", "description");
		group.createBool("4", "Bool", false, function() {}, true);
	})(settings, prop)`)
	if h.numericKeySeen {
		t.Fatal("globalSettings converted a named numeric ID to an index")
	}
	if h.stringNumericIDs["3"] == 0 || h.stringNumericIDs["4"] == 0 {
		t.Fatalf("global setting IDs were not passed as string children: %v", h.stringNumericIDs)
	}
}

func TestMovianPropNumericLiteralStillUsesIndexedLookup(t *testing.T) {
	h := newSettingsHarness(t, false)
	h.run(t, `var nodes = prop.makeProp({}); nodes[0];`)
	if !h.numericKeySeen {
		t.Fatal("numeric literal property access did not use the native indexed-child lookup")
	}
}

func loadTestModule(t *testing.T, vm *goja.Runtime, source []byte, require func(string) *goja.Object) *goja.Object {
	t.Helper()
	wrapperValue, err := vm.RunString("(function(exports, require) {" + string(source) + "\n})")
	if err != nil {
		t.Fatalf("compile module: %v", err)
	}
	wrapper, ok := goja.AssertFunction(wrapperValue)
	if !ok {
		t.Fatal("module wrapper is not callable")
	}
	exports := vm.NewObject()
	if _, err := wrapper(goja.Undefined(), exports, vm.ToValue(require)); err != nil {
		t.Fatalf("load module: %v", err)
	}
	return exports
}
