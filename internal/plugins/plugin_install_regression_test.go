package plugins

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fileaccesscore "github.com/czz/movian-go/internal/fileaccess"
	"github.com/czz/movian-go/internal/gconf"
	propcore "github.com/czz/movian-go/internal/prop"
	settingscore "github.com/czz/movian-go/internal/settings"
	"github.com/czz/movian-go/internal/trace"
)

func newArchiveRegressionPluginManager(t *testing.T) (*PluginManager, string) {
	t.Helper()
	return newArchiveRegressionPluginManagerAt(t, t.TempDir())
}

func newArchiveRegressionPluginManagerAt(t *testing.T, dataDir string) (*PluginManager, string) {
	t.Helper()
	pm := NewPluginManager(nil, nil, nil)
	previousPath := getPersistentPath()
	pm.SetPersistentPath(dataDir)
	t.Cleanup(func() { pm.SetPersistentPath(previousPath) })

	gc := gconf.New()
	gc.CachePath = dataDir
	pm.SetGconf(gc)
	fam := fileaccesscore.NewFileAccessManager(nil, nil)
	fam.SetGconf(gc)
	fam.RegisterProtocols()
	pm.SetFileAccessManager(fam)
	pm.SetPropManager(propcore.NewPropManager())
	return pm, dataDir
}

func writeRegressionPlugin(t *testing.T, path, id, pluginType, version string, withView bool) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create("plugin.json")
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	manifest := PluginControl{ID: id, Type: pluginType, Version: version, Title: id + " " + version}
	if pluginType == "ecmascript" {
		manifest.File = "main.js"
	}
	if withView {
		manifest.GLWViews = []*PluginViewControl{{
			Class: "home",
			Title: "Home view",
			File:  "home.view",
		}}
	}
	if err := json.NewEncoder(entry).Encode(manifest); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if pluginType == "ecmascript" {
		script, err := archive.Create(manifest.File)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if _, err := io.WriteString(script, "var lifecycleReady = true;"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if withView {
		for _, resource := range []struct {
			name string
			data string
		}{
			{name: "home.view", data: "rollback view"},
			{name: "helper.js", data: "exports.value = 'rollback module';"},
		} {
			resourceFile, err := archive.Create(resource.name)
			if err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if _, err := io.WriteString(resourceFile, resource.data); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
		}
	}

	if err := archive.Close(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func regressionArchiveVersion(t *testing.T, path string) string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.Name != "plugin.json" {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(entry)
		closeErr := entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		var manifest PluginControl
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest.Version
	}
	t.Fatal("plugin archive does not contain plugin.json")
	return ""
}

func TestRepositoryPluginUsesResolvedPackageURLOrigin(t *testing.T) {
	pm := NewPluginManager(nil, nil, nil)
	repo := &PluginRepo{URL: "https://example.test/plugins/repository.json"}
	ctrl := &PluginControl{ID: "hashed-plugin", Type: "views", Version: "1.0", DownloadURL: "demo.zip"}

	pm.mu.Lock()
	err := pm.loadRepoPlugin(repo, ctrl)
	pm.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	const packageURL = "https://example.test/plugins/demo.zip"
	const origin = "6260e364e9c16db6"
	plugin := pm.FindPlugin("hashed-plugin@" + origin)
	if plugin == nil || plugin.Origin != origin || plugin.Package != packageURL {
		t.Fatalf("repository identity = %#v; want package %q and origin %q", plugin, packageURL, origin)
	}
}

func TestExistingPluginPointerUpdatePreservesIdentityAndMetadata(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	packagePath := filepath.Join(dataDir, "repo-package.zip")
	packageURL := "file://" + filepath.ToSlash(packagePath)
	origin := originHash("https://example.test/plugins/repo-package.zip")
	plugin := pm.MakePlugin("target-plugin", origin)
	plugin.Package = "https://example.test/plugins/repo-package.zip"

	writeRegressionPlugin(t, packagePath, "target-plugin", "views", "1.0", false)
	if err := pm.InstallPlugin(plugin, packageURL); err != nil {
		t.Fatal(err)
	}
	unloaded := 0
	plugin.Unload = func(*Plugin) { unloaded++ }
	writeRegressionPlugin(t, packagePath, "target-plugin", "views", "2.0", false)
	if err := pm.InstallPlugin(plugin, packageURL); err != nil {
		t.Fatal(err)
	}
	if unloaded != 1 {
		t.Fatalf("target runtime unload count = %d, want one", unloaded)
	}

	if pm.FindPlugin(plugin.FQID) != plugin || plugin.FQID != "target-plugin@"+origin ||
		plugin.Origin != origin || plugin.Package != "https://example.test/plugins/repo-package.zip" ||
		plugin.InstVer != "2.0" || !plugin.Installed || !plugin.Loaded {
		t.Fatalf("target plugin after update = %#v", plugin)
	}
	if pm.FindPlugin("target-plugin@local") != nil || len(pm.GetPlugins()) != 1 {
		t.Fatalf("update created a duplicate/local plugin: %v", pm.GetPlugins())
	}
	installedPath := filepath.Join(dataDir, "mrp", "installed", plugin.FQID+".zip")
	if version := regressionArchiveVersion(t, installedPath); version != "2.0" {
		t.Fatalf("installed target archive version = %q, want 2.0", version)
	}
}

func TestExistingPluginPointerUpdateRejectsManifestIDMismatch(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	packagePath := filepath.Join(dataDir, "target.zip")
	packageURL := "file://" + filepath.ToSlash(packagePath)
	plugin := pm.MakePlugin("target-plugin", "repo-origin")
	plugin.Package = packageURL
	writeRegressionPlugin(t, packagePath, "target-plugin", "views", "1.0", false)
	if err := pm.InstallPlugin(plugin, packageURL); err != nil {
		t.Fatal(err)
	}

	writeRegressionPlugin(t, packagePath, "other-plugin", "views", "2.0", false)
	err := pm.InstallPlugin(plugin, packageURL)
	if err == nil || !strings.Contains(err.Error(), "does not match target") {
		t.Fatalf("manifest mismatch error = %v, want target mismatch", err)
	}
	if plugin.InstVer != "1.0" || !plugin.Loaded {
		t.Fatalf("target state changed after manifest mismatch: %#v", plugin)
	}
	installedPath := filepath.Join(dataDir, "mrp", "installed", plugin.FQID+".zip")
	if version := regressionArchiveVersion(t, installedPath); version != "1.0" {
		t.Fatalf("archive after manifest mismatch = %q, want 1.0", version)
	}
	if pm.FindPlugin("other-plugin@local") != nil {
		t.Fatal("manifest mismatch created a local plugin")
	}
}

func TestConcurrentLocalInstallAndUninstallLeaveConsistentStorage(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	packagePath := filepath.Join(dataDir, "source.zip")
	packageURL := "file://" + filepath.ToSlash(packagePath)
	writeRegressionPlugin(t, packagePath, "transaction-plugin", "views", "1.0", false)
	if err := pm.InstallPluginFromURL(packageURL); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin("transaction-plugin@local")
	writeRegressionPlugin(t, packagePath, "transaction-plugin", "views", "2.0", false)

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- pm.InstallPluginFromURL(packageURL) }()
	go func() { <-start; results <- pm.RemovePlugin(plugin) }()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	installedPath := filepath.Join(dataDir, "mrp", "installed", plugin.FQID+".zip")
	_, statErr := os.Stat(installedPath)
	if plugin.Installed != (statErr == nil) || plugin.Loaded != plugin.Installed {
		t.Fatalf("install/remove left inconsistent state: installed=%v loaded=%v stat=%v",
			plugin.Installed, plugin.Loaded, statErr)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	if plugin.Installed && plugin.InstVer != "2.0" {
		t.Fatalf("concurrent install left version %q, want 2.0", plugin.InstVer)
	}
}

func TestRollbackPreservesPreviouslySelectedPluginView(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	settingsManager := settingscore.NewSettingsManager(pm.GetPropManager())
	settingsManager.SetGconf(gconf.New())
	settingsManager.Start()
	pm.SetSettingsManager(settingsManager)
	pm.pluginsViewSettingsSetup()

	firstPath := filepath.Join(dataDir, "first-view-plugin.zip")
	secondPath := filepath.Join(dataDir, "second-view-plugin.zip")
	firstURL := "file://" + filepath.ToSlash(firstPath)
	secondURL := "file://" + filepath.ToSlash(secondPath)
	writeRegressionPlugin(t, firstPath, "first-view", "views", "1.0", true)
	writeRegressionPlugin(t, secondPath, "second-view", "views", "1.0", true)
	if err := OpenFile(context.Background(), pm, firstURL); err != nil {
		t.Fatal(err)
	}
	if err := OpenFile(context.Background(), pm, secondURL); err != nil {
		t.Fatal(err)
	}
	homeViewProp := pm.GetPropManager().GetGlobal()
	for _, part := range []string{"glw", "views", "standard", "home"} {
		homeViewProp = homeViewProp.FindChild(part)
		if homeViewProp == nil {
			t.Fatalf("missing global view property %q", part)
		}
	}
	secondViews := pm.GetViewsByPlugin("second-view@local")
	if len(secondViews) == 0 {
		t.Fatal("second plugin view was not registered")
	}
	secondView := secondViews[0]

	selectionChanged := make(chan struct{}, 1)
	selectionSub := homeViewProp.Subscribe(func(_ any, event propcore.EventType, _ ...any) {
		if event == propcore.EventSelectChild {
			select {
			case selectionChanged <- struct{}{}:
			default:
			}
		}
	}, nil, propcore.SubFlagInternal, propcore.SubNoInitialUpdate)
	defer selectionSub.Unsubscribe()

	pm.SelectView("second-view@local", "home.view")
	selectionTimeout := time.NewTimer(5 * time.Second)
	defer selectionTimeout.Stop()
	var selected *propcore.Prop
	for {
		selected = homeViewProp.GetSelectedChild()
		if selected != nil && selected.GetName() == secondView.Fullpath {
			break
		}
		select {
		case <-selectionChanged:
		case <-selectionTimeout.C:
			t.Fatalf("view selection did not reach %q; selected=%v", secondView.Fullpath, selected)
		}
	}

	writeRegressionPlugin(t, firstPath, "first-view", "unsupported", "2.0", false)
	if err := OpenFile(context.Background(), pm, firstURL); err == nil {
		t.Fatal("unsupported replacement unexpectedly installed")
	}
	selected = homeViewProp.GetSelectedChild()
	if selected == nil || selected.GetName() != secondView.Fullpath {
		t.Fatalf("rollback changed selected view to %v, want %q", selected, secondView.Fullpath)
	}
}

func TestRollbackRestoresViewOwnedByFailedPlugin(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	sm := settingscore.NewSettingsManager(pm.GetPropManager())
	sm.SetGconf(gconf.New())
	sm.Start()
	pm.SetSettingsManager(sm)
	pm.pluginsViewSettingsSetup()

	path := filepath.Join(dataDir, "owner.zip")
	packageURL := "file://" + filepath.ToSlash(path)
	writeRegressionPlugin(t, path, "view-owner", "views", "1.0", true)
	if err := OpenFile(context.Background(), pm, packageURL); err != nil {
		t.Fatal(err)
	}

	pl := pm.FindPlugin("view-owner@local")
	if pl == nil || len(pl.Views) == 0 || pl.Views[0].SettingProp == nil {
		t.Fatal("original plugin view was not registered")
	}
	original := pl.Views[0]
	pm.SelectView(pl.FQID, original.Filename)

	// View selection can propagate through a settings courier.
	waitForSelection := func(want *propcore.Prop) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			parent := want.GetParent()
			if parent != nil && parent.GetSelectedChild() == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("selected view did not become %q", want.GetName())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForSelection(original.TypeProp)
	waitForSelection(original.SettingProp)

	// Loading this replacement fails after the previous view was unloaded.
	writeRegressionPlugin(t, path, "view-owner", "unsupported", "2.0", false)
	if err := OpenFile(context.Background(), pm, packageURL); err == nil {
		t.Fatal("unsupported replacement unexpectedly installed")
	}
	if len(pl.Views) != 1 || pl.Views[0].TypeProp == nil || pl.Views[0].SettingProp == nil {
		t.Fatalf("rollback did not restore the original view: %#v", pl.Views)
	}
	restored := pl.Views[0]
	waitForSelection(restored.TypeProp)
	waitForSelection(restored.SettingProp)
	if pl.InstVer != "1.0" || !pl.Installed || !pl.Loaded {
		t.Fatalf("plugin state after rollback: %#v", pl)
	}
	stored := filepath.Join(dataDir, "mrp", "installed", pl.FQID+".zip")
	if got := regressionArchiveVersion(t, stored); got != "1.0" {
		t.Fatalf("rollback archive version = %q, want 1.0", got)
	}
}
func TestFailedReplacementReloadsLoadedUninstalledPlugin(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "loaded-repository-plugin"
		origin   = "repository-origin"
	)
	oldPath := filepath.Join(dataDir, "old-package.zip")
	newPath := filepath.Join(dataDir, "new-package.zip")
	oldURL := "file://" + filepath.ToSlash(oldPath)
	newURL := "file://" + filepath.ToSlash(newPath)
	writeRegressionPlugin(t, oldPath, pluginID, "views", "1.0", true)
	writeRegressionPlugin(t, newPath, pluginID, "unsupported", "2.0", false)
	oldZipPath, err := resolveZipPath(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.LoadPlugin(oldZipPath, origin, 0); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(pluginID + "@" + origin)
	if plugin == nil || !plugin.Loaded || plugin.Installed {
		t.Fatalf("initial repository plugin state = %#v", plugin)
	}
	pm.mu.Lock()
	plugin.Package = oldURL
	pm.mu.Unlock()

	if err := pm.InstallPlugin(plugin, newURL); err == nil {
		t.Fatal("unsupported replacement unexpectedly installed")
	}
	if got := pm.FindPlugin(pluginID + "@" + origin); got != plugin ||
		!plugin.Loaded || plugin.Installed || plugin.Package != oldURL ||
		len(plugin.Views) != 1 {
		t.Fatalf("failed replacement lost loaded plugin state: %#v", plugin)
	}
}

func TestFailedReplacementKeepsRollbackArchiveUntilUnload(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "loaded-repository-plugin"
		origin   = "repository-origin"
	)
	oldPath := filepath.Join(dataDir, "old-package.zip")
	newPath := filepath.Join(dataDir, "new-package.zip")
	oldURL := "file://" + filepath.ToSlash(oldPath)
	newURL := "file://" + filepath.ToSlash(newPath)
	writeRegressionPlugin(t, oldPath, pluginID, "views", "1.0", true)
	writeRegressionPlugin(t, newPath, pluginID, "unsupported", "2.0", false)

	oldZipPath, err := resolveZipPath(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.LoadPlugin(oldZipPath, origin, 0); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(pluginID + "@" + origin)
	if plugin == nil || !plugin.Loaded || plugin.Installed {
		t.Fatalf("initial repository plugin state = %#v", plugin)
	}
	pm.mu.Lock()
	plugin.Package = oldURL
	pm.mu.Unlock()

	if err := pm.InstallPlugin(plugin, newURL); err == nil {
		t.Fatal("unsupported replacement unexpectedly installed")
	}
	if got := pm.FindPlugin(pluginID + "@" + origin); got != plugin ||
		!plugin.Loaded || plugin.Installed || plugin.Origin != origin ||
		plugin.FQID != pluginID+"@"+origin || plugin.Package != oldURL ||
		len(plugin.Views) != 1 {
		t.Fatalf("failed replacement lost repository plugin state: %#v", plugin)
	}

	viewURI := plugin.Views[0].View.Fullpath
	zipURI := strings.TrimPrefix(viewURI, "zip://")
	zipSuffix := strings.Index(zipURI, ".zip/")
	if zipSuffix < 0 {
		t.Fatalf("restored view URI %q does not identify its archive", viewURI)
	}
	archivePath := zipURI[:zipSuffix+len(".zip")]
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("rollback archive %q is not available after InstallPlugin returned: %v", archivePath, err)
	}

	for _, resource := range []struct {
		uri  string
		want string
	}{
		{uri: viewURI, want: "rollback view"},
		{uri: strings.TrimSuffix(viewURI, "/home.view") + "/helper.js",
			want: "exports.value = 'rollback module';"},
	} {
		got, err := loadFileWithFA(resource.uri, pm.faManager)
		if err != nil {
			t.Fatalf("open restored ZIP resource %q: %v", resource.uri, err)
		}
		if string(got) != resource.want {
			t.Fatalf("resource %q = %q, want %q", resource.uri, got, resource.want)
		}
	}

	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	if err := pm.InstallPlugin(plugin, newURL); err == nil {
		t.Fatal("second unsupported replacement unexpectedly installed")
	}
	if !plugin.Loaded || plugin.FQID != pluginID+"@"+origin {
		t.Fatalf("second rollback lost plugin identity or loaded state: %#v", plugin)
	}
	rollbackArchives, err := filepath.Glob(filepath.Join(dataDir, "plugin-rollback-*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rollbackArchives) != 1 || rollbackArchives[0] != archivePath {
		t.Fatalf("rollback cache after repeated failure = %v, want only %q", rollbackArchives, archivePath)
	}
	if _, err := loadFileWithFA(plugin.Views[0].View.Fullpath, pm.faManager); err != nil {
		t.Fatalf("view resource unavailable after repeated rollback: %v", err)
	}

	pm.UnloadPlugin(plugin)
	if plugin.Loaded {
		t.Fatal("plugin remains loaded after unload")
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("rollback archive remains after plugin unload: %q, err=%v", archivePath, err)
	}
}

func TestRecoverPluginInstallTransactionCrashStates(t *testing.T) {
	const (
		pluginID = "transaction-state-plugin"
		origin   = "fedcba9876543210"
		fqid     = pluginID + "@" + origin
	)
	for _, tc := range []struct {
		name        string
		phase       byte
		hadBackup   bool
		current     string
		backup      string
		wantVersion string
		wantAbsent  bool
	}{
		{name: "prepared replacement leaves old archive", phase: pluginInstallPrepared,
			hadBackup: true, current: "1.0", wantVersion: "1.0"},
		{name: "interrupted replacement restores backup", phase: pluginInstallReplacing,
			hadBackup: true, current: "2.0", backup: "1.0", wantVersion: "1.0"},
		{name: "committed replacement keeps new archive", phase: pluginInstallCommitted,
			hadBackup: true, current: "2.0", backup: "1.0", wantVersion: "2.0"},
		{name: "interrupted first install removes new archive", phase: pluginInstallReplacing,
			current: "2.0", wantAbsent: true},
		{name: "committed first install keeps new archive", phase: pluginInstallCommitted,
			current: "2.0", wantVersion: "2.0"},
		{name: "completed rollback with stale journal is idempotent", phase: pluginInstallReplacing,
			hadBackup: true, current: "1.0", wantVersion: "1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm, dataDir := newArchiveRegressionPluginManager(t)
			installDir := filepath.Join(dataDir, "mrp", "installed")
			if err := os.MkdirAll(installDir, 0755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(installDir, fqid+".zip")
			txnDir, err := createPluginInstallTransaction(installDir, filepath.Base(target), tc.hadBackup)
			if err != nil {
				t.Fatal(err)
			}
			if tc.current != "" {
				writeRegressionPlugin(t, target, pluginID, "views", tc.current, false)
			}
			if tc.backup != "" {
				writeRegressionPlugin(t, filepath.Join(txnDir, "backup.zip"),
					pluginID, "views", tc.backup, false)
			}
			if err := writePluginInstallPhase(txnDir, tc.phase); err != nil {
				t.Fatal(err)
			}

			if err := pm.LoadInstalledPlugins(dataDir); err != nil {
				t.Fatal(err)
			}
			plugin := pm.FindPlugin(fqid)
			if tc.wantAbsent {
				if plugin != nil {
					t.Fatalf("uncommitted first install loaded: %#v", plugin)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("uncommitted archive remains: %v", err)
				}
			} else if plugin == nil || !plugin.Loaded || plugin.InstVer != tc.wantVersion {
				t.Fatalf("recovered plugin = %#v, want version %s", plugin, tc.wantVersion)
			}
			if _, err := os.Stat(txnDir); !os.IsNotExist(err) {
				t.Fatalf("transaction journal remains after recovery: %v", err)
			}
		})
	}
}

func TestReplacingTransactionPreservesJournalWhenBackupCannotBeRead(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "unreadable-backup-plugin"
		origin   = "fedcba9876543210"
	)
	installDir := filepath.Join(dataDir, "mrp", "installed")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(installDir, pluginID+"@"+origin+".zip")
	writeRegressionPlugin(t, target, pluginID, "views", "2.0", false)
	txnDir, err := createPluginInstallTransaction(installDir, filepath.Base(target), true)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(txnDir, "backup.zip")
	if err := os.Mkdir(backupPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writePluginInstallPhase(txnDir, pluginInstallReplacing); err != nil {
		t.Fatal(err)
	}

	if err := pm.recoverInterruptedPluginInstalls(installDir); err == nil {
		t.Fatal("recovery hid unreadable transaction backup")
	}
	if got := regressionArchiveVersion(t, target); got != "2.0" {
		t.Fatalf("current archive after failed recovery = %q, want 2.0", got)
	}
	if _, err := os.Stat(txnDir); err != nil {
		t.Fatalf("journal removed despite unreadable backup: %v", err)
	}

	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, backupPath, pluginID, "views", "1.0", false)
	if err := pm.recoverInterruptedPluginInstalls(installDir); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	if got := regressionArchiveVersion(t, target); got != "1.0" {
		t.Fatalf("archive after retry = %q, want restored 1.0 backup", got)
	}
	if _, err := os.Stat(txnDir); !os.IsNotExist(err) {
		t.Fatalf("journal remains after successful retry: %v", err)
	}
}

func TestCommittedTransactionCleanupFailureIsRetriedIdempotently(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "retry-cleanup-plugin"
		origin   = "fedcba9876543210"
	)
	installDir := filepath.Join(dataDir, "mrp", "installed")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(installDir, pluginID+"@"+origin+".zip")
	writeRegressionPlugin(t, target, pluginID, "views", "2.0", false)
	txnDir, err := createPluginInstallTransaction(installDir, filepath.Base(target), true)
	if err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(txnDir, "backup.zip"), pluginID, "views", "1.0", false)
	if err := writePluginInstallPhase(txnDir, pluginInstallCommitted); err != nil {
		t.Fatal(err)
	}

	injected := false
	pm.removeInstallDir = func(path string) error {
		if !injected {
			injected = true
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	if err := pm.recoverInterruptedPluginInstalls(installDir); err == nil {
		t.Fatal("recovery hid the transaction cleanup failure")
	}
	if got := regressionArchiveVersion(t, target); got != "2.0" {
		t.Fatalf("committed version after cleanup failure = %q, want 2.0", got)
	}
	if _, err := os.Stat(txnDir); err != nil {
		t.Fatalf("transaction needed for retry was removed: %v", err)
	}
	if err := pm.recoverInterruptedPluginInstalls(installDir); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	if _, err := os.Stat(txnDir); !os.IsNotExist(err) {
		t.Fatalf("transaction remains after successful retry: %v", err)
	}
	if got := regressionArchiveVersion(t, target); got != "2.0" {
		t.Fatalf("version after idempotent retry = %q, want 2.0", got)
	}
}

func TestRecoveryRejectsInvalidBackupWithoutReplacingValidInstall(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "valid-transaction-plugin"
		origin   = "fedcba9876543210"
	)
	installDir := filepath.Join(dataDir, "mrp", "installed")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(installDir, pluginID+"@"+origin+".zip")
	writeRegressionPlugin(t, target, pluginID, "views", "2.0", false)
	txnDir, err := createPluginInstallTransaction(installDir, filepath.Base(target), true)
	if err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(txnDir, "backup.zip"), "wrong-plugin", "views", "1.0", false)
	if err := writePluginInstallPhase(txnDir, pluginInstallReplacing); err != nil {
		t.Fatal(err)
	}

	if err := pm.recoverInterruptedPluginInstalls(installDir); err == nil {
		t.Fatal("invalid transaction backup was not surfaced")
	}
	if got := regressionArchiveVersion(t, target); got != "2.0" {
		t.Fatalf("invalid backup replaced valid install: got version %q, want 2.0", got)
	}
	if _, err := os.Stat(txnDir); !os.IsNotExist(err) {
		t.Fatalf("invalid transaction with a valid install was not cleaned up: %v", err)
	}
}

func TestCopyPluginArchiveBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		want     int64
		tooLarge bool
	}{
		{name: "below", data: "abc", want: 3},
		{name: "exact", data: "abcd", want: 4},
		{name: "above", data: "abcdef", want: 5, tooLarge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := copyPluginArchiveBounded(io.Discard, strings.NewReader(tc.data), 4)
			if got != tc.want {
				t.Fatalf("bytes read = %d, want %d", got, tc.want)
			}
			if tc.tooLarge {
				if err == nil || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("expected package-size error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOversizedPluginDownloadDoesNotLeaveCacheFile(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	sourcePath := filepath.Join(dataDir, "oversized-source.zip")
	f, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPluginDownloadBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = pm.downloadPlugin("file://" + filepath.ToSlash(sourcePath))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized download should fail before installation: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(dataDir, "plugin-*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("oversized download left temporary files: %v", temps)
	}
}

func TestLoadInstalledPluginsRetainsFQIDFromArchiveName(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const origin = "fedcba9876543210"
	installedPath := filepath.Join(dataDir, "mrp", "installed", "startup-plugin@"+origin+".zip")
	if err := os.MkdirAll(filepath.Dir(installedPath), 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, installedPath, "startup-plugin", "views", "1.0", false)
	if err := pm.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin("startup-plugin@" + origin)
	if plugin == nil || !plugin.Installed || !plugin.Loaded || plugin.Origin != origin {
		t.Fatalf("startup-loaded plugin = %#v", plugin)
	}
}
func TestLoadInstalledPluginsRecoversInterruptedReplacement(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "recovered-plugin"
		origin   = "fedcba9876543210"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	fqid := pluginID + "@" + origin
	installedPath := filepath.Join(installedDir, fqid+".zip")
	backupDir := filepath.Join(installedDir, ".plugin-backup-interrupted")
	backupPath := filepath.Join(backupDir, fqid+".zip")
	stagedPath := filepath.Join(installedDir, ".plugin-install-interrupted.zip")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, backupPath, pluginID, "views", "1.0", false)
	if err := os.WriteFile(installedPath, []byte("incomplete replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, []byte("incomplete staging file"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := pm.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(fqid)
	if plugin == nil || !plugin.Loaded || !plugin.Installed || plugin.Origin != origin ||
		plugin.InstVer != "1.0" {
		t.Fatalf("recovered plugin = %#v", plugin)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "1.0" {
		t.Fatalf("recovered archive version = %q, want 1.0", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("backup directory remains after recovery: %v", err)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged archive remains after recovery: %v", err)
	}

	// Model termination after the backup was renamed back but before its
	// now-empty transaction directory was removed.
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	pm2, _ := newArchiveRegressionPluginManagerAt(t, dataDir)
	if err := pm2.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatalf("repeated startup recovery: %v", err)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("empty recovery directory remains: %v", err)
	}
	plugin = pm2.FindPlugin(fqid)
	if plugin == nil || !plugin.Loaded || !plugin.Installed || plugin.InstVer != "1.0" {
		t.Fatalf("recovered plugin after restart = %#v", plugin)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "1.0" {
		t.Fatalf("archive changed on repeated recovery = %q, want 1.0", got)
	}
}
func TestLoadInstalledPluginsPrefersValidCurrentArchiveOverLegacyBackup(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "legacy-backup-plugin"
		origin   = "fedcba9876543210"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	fqid := pluginID + "@" + origin
	installedPath := filepath.Join(installedDir, fqid+".zip")
	backupDir := filepath.Join(installedDir, ".plugin-backup-cleanup-failed")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(backupDir, fqid+".zip"), pluginID, "views", "1.0", false)
	writeRegressionPlugin(t, installedPath, pluginID, "views", "2.0", false)

	if err := pm.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(fqid)
	if plugin == nil || !plugin.Loaded || !plugin.Installed || plugin.Origin != origin ||
		plugin.InstVer != "2.0" {
		t.Fatalf("valid current plugin archive was downgraded: %#v", plugin)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("current archive version = %q, want 2.0", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("stale legacy backup remains: %v", err)
	}
}

func TestLegacyRecoveryPreservesValidCurrentArchiveAcrossCleanupFailure(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "legacy-cleanup-plugin"
		origin   = "fedcba9876543210"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	fqid := pluginID + "@" + origin
	installedPath := filepath.Join(installedDir, fqid+".zip")
	backupDir := filepath.Join(installedDir, ".plugin-backup-cleanup-retry")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(backupDir, fqid+".zip"), pluginID, "views", "1.0", false)
	writeRegressionPlugin(t, installedPath, pluginID, "views", "2.0", false)

	removeCount := 0
	pm.removeInstallDir = func(path string) error {
		removeCount++
		if removeCount == 1 {
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	if err := pm.recoverInterruptedPluginInstalls(installedDir); err == nil {
		t.Fatal("legacy backup cleanup failure was not returned")
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("current version after cleanup failure = %q, want 2.0", got)
	}
	if _, err := os.Stat(filepath.Join(backupDir, filepath.Base(installedPath))); err != nil {
		t.Fatalf("backup should remain available for cleanup retry: %v", err)
	}

	if err := pm.recoverInterruptedPluginInstalls(installedDir); err != nil {
		t.Fatalf("retry legacy backup cleanup: %v", err)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("current version after cleanup retry = %q, want 2.0", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("legacy backup remains after cleanup retry: %v", err)
	}
}

func TestLegacyRecoveryRestoresBackupWhenCurrentArchiveIsMissing(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "legacy-missing-current-plugin"
		origin   = "fedcba9876543210"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	fqid := pluginID + "@" + origin
	installedPath := filepath.Join(installedDir, fqid+".zip")
	backupDir := filepath.Join(installedDir, ".plugin-backup-missing-current")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(backupDir, fqid+".zip"), pluginID, "views", "1.0", false)

	if err := pm.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(fqid)
	if plugin == nil || !plugin.Loaded || !plugin.Installed || plugin.InstVer != "1.0" {
		t.Fatalf("missing-current recovery = %#v, want previous version 1.0", plugin)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "1.0" {
		t.Fatalf("restored archive version = %q, want 1.0", got)
	}
}

func TestLoadInstalledPluginsDoesNotReplaceValidArchiveWithInvalidBackup(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "valid-plugin"
		origin   = "fedcba9876543210"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	fqid := pluginID + "@" + origin
	installedPath := filepath.Join(installedDir, fqid+".zip")
	backupDir := filepath.Join(installedDir, ".plugin-backup-invalid")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeRegressionPlugin(t, filepath.Join(backupDir, fqid+".zip"), "wrong-plugin", "views", "1.0", false)
	writeRegressionPlugin(t, installedPath, pluginID, "views", "2.0", false)

	if err := pm.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(fqid)
	if plugin == nil || !plugin.Loaded || !plugin.Installed || plugin.Origin != origin ||
		plugin.InstVer != "2.0" {
		t.Fatalf("valid current plugin archive was not retained: %#v", plugin)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("retained archive version = %q, want 2.0", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("invalid backup directory remains: %v", err)
	}
}

func TestRepositoryUpgradePreservesFQID(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	packagePath := filepath.Join(dataDir, "repo-upgrade.zip")
	packageURL := "file://" + filepath.ToSlash(packagePath)
	origin := originHash("https://example.test/plugins/repo-upgrade.zip")
	plugin := pm.MakePlugin("repo-upgrade", origin)
	plugin.Package = packageURL
	writeRegressionPlugin(t, packagePath, "repo-upgrade", "views", "1.0", false)
	if err := pm.InstallPlugin(plugin, packageURL); err != nil {
		t.Fatal(err)
	}
	plugin.CanUpgrade = true
	writeRegressionPlugin(t, packagePath, "repo-upgrade", "views", "2.0", false)
	if err := pm.UpgradePlugin(plugin); err != nil {
		t.Fatal(err)
	}
	if plugin.FQID != "repo-upgrade@"+origin || plugin.Origin != origin ||
		plugin.InstVer != "2.0" || !plugin.Installed || !plugin.Loaded {
		t.Fatalf("repository upgrade state = %#v", plugin)
	}
	if pm.FindPlugin("repo-upgrade@local") != nil {
		t.Fatal("repository upgrade created a local plugin")
	}
}

func TestLocalInstallDoesNotMergeDifferentOrigin(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	repositoryPlugin := pm.MakePlugin("same-id", "repository-origin")
	repositoryPlugin.Title = "Repository instance"
	packagePath := filepath.Join(dataDir, "local.zip")
	packageURL := "file://" + filepath.ToSlash(packagePath)
	writeRegressionPlugin(t, packagePath, "same-id", "views", "1.0", false)
	if err := OpenFile(context.Background(), pm, packageURL); err != nil {
		t.Fatal(err)
	}
	localPlugin := pm.FindPlugin("same-id@local")
	if localPlugin == nil || !localPlugin.Installed || !localPlugin.Loaded {
		t.Fatalf("local plugin was not installed: %#v", localPlugin)
	}
	if err := OpenFile(context.Background(), pm, packageURL); err != nil {
		t.Fatal(err)
	}
	if pm.FindPlugin("same-id@local") != localPlugin {
		t.Fatal("same-origin local reinstall replaced its plugin record")
	}
	if pm.FindPlugin(repositoryPlugin.FQID) != repositoryPlugin || repositoryPlugin.Installed ||
		repositoryPlugin.Title != "Repository instance" {
		t.Fatalf("local install merged into repository plugin: %#v", repositoryPlugin)
	}
	if len(pm.GetPlugins()) != 2 {
		t.Fatalf("plugins after local install = %v, want two identities", pm.GetPlugins())
	}
}

func TestCommittedReplacementSurvivesBackupCleanupFailureAndRestart(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	const (
		pluginID = "cleanup-failure-plugin"
		origin   = "repository-origin"
	)
	installedDir := filepath.Join(dataDir, "mrp", "installed")
	if err := os.MkdirAll(installedDir, 0755); err != nil {
		t.Fatal(err)
	}
	installedPath := filepath.Join(installedDir, pluginID+"@"+origin+".zip")
	oldPath := filepath.Join(dataDir, "old-package.zip")
	newPath := filepath.Join(dataDir, "new-package.zip")
	writeRegressionPlugin(t, installedPath, pluginID, "views", "1.0", true)
	writeRegressionPlugin(t, oldPath, pluginID, "views", "1.0", true)
	writeRegressionPlugin(t, newPath, pluginID, "views", "2.0", true)

	oldZipPath, err := resolveZipPath(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.LoadPlugin(oldZipPath, origin, 0); err != nil {
		t.Fatal(err)
	}
	plugin := pm.FindPlugin(pluginID + "@" + origin)
	if plugin == nil {
		t.Fatal("loaded plugin not found")
	}

	injected := false
	pm.removeInstallDir = func(path string) error {
		if !injected {
			injected = true
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	newURL := "file://" + filepath.ToSlash(newPath)
	if err := pm.InstallPlugin(plugin, newURL); err != nil {
		t.Fatalf("committed installation returned error after cleanup failure: %v", err)
	}
	if !injected {
		t.Fatal("backup cleanup failure was not injected")
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("installed version after commit = %q, want 2.0", got)
	}

	restarted, _ := newArchiveRegressionPluginManagerAt(t, dataDir)
	if err := restarted.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	recovered := restarted.FindPlugin(pluginID + "@" + origin)
	if recovered == nil || !recovered.Loaded || !recovered.Installed ||
		recovered.InstVer != "2.0" {
		t.Fatalf("committed version did not survive restart: %#v", recovered)
	}
	if got := regressionArchiveVersion(t, installedPath); got != "2.0" {
		t.Fatalf("version after restart = %q, want committed 2.0", got)
	}
}
func TestInstalledPluginIDsWithInternalPrefixesSurviveRestart(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	for _, id := range []string{".plugin-install-example", ".plugin-backup-example"} {
		packagePath := filepath.Join(dataDir, strings.TrimPrefix(id, ".")+"-source.zip")
		writeRegressionPlugin(t, packagePath, id, "views", "1.0", false)
		plugin := pm.MakePlugin(id, "custom-origin")
		if err := pm.InstallPlugin(plugin, "file://"+filepath.ToSlash(packagePath)); err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
	}

	restarted, _ := newArchiveRegressionPluginManagerAt(t, dataDir)
	if err := restarted.LoadInstalledPlugins(dataDir); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{".plugin-install-example", ".plugin-backup-example"} {
		plugin := restarted.FindPlugin(id + "@custom-origin")
		if plugin == nil || !plugin.Loaded || !plugin.Installed {
			t.Fatalf("plugin %q was skipped during recovery/startup: %#v", id, plugin)
		}
	}
}

func TestRecoveryErrorIsReturnedWhileHealthyPluginsLoad(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	installDir := filepath.Join(dataDir, "mrp", "installed")
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	txnDir, err := createPluginInstallTransaction(installDir, "broken@repo.zip", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePluginInstallPhase(txnDir, pluginInstallReplacing); err != nil {
		t.Fatal(err)
	}
	healthyPath := filepath.Join(installDir, "healthy@repo.zip")
	writeRegressionPlugin(t, healthyPath, "healthy", "views", "1.0", false)

	err = pm.LoadInstalledPlugins(dataDir)
	if err == nil {
		t.Fatal("invalid transaction recovery error was not surfaced")
	}
	var recoveryErr *PluginRecoveryError
	if !errors.As(err, &recoveryErr) || recoveryErr.FQID != "broken@repo" {
		t.Fatalf("recovery error = %#v, want structured error for broken@repo", recoveryErr)
	}
	healthy := pm.FindPlugin("healthy@repo")
	if healthy == nil || !healthy.Loaded || !healthy.Installed {
		t.Fatalf("healthy plugin was not loaded after isolated recovery failure: %#v", healthy)
	}
}

func TestBlockedDownloadAllowsOtherPluginsAndRejectsConcurrentSameFQID(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	firstArchive := filepath.Join(dataDir, "first.zip")
	unrelatedArchive := filepath.Join(dataDir, "unrelated.zip")
	writeRegressionPlugin(t, firstArchive, "blocked-target", "views", "1.0", false)
	writeRegressionPlugin(t, unrelatedArchive, "unrelated-target", "views", "1.0", false)
	firstBytes, err := os.ReadFile(firstArchive)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDownload := func() { releaseOnce.Do(func() { close(release) }) }
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		close(started)
		<-release
		_, _ = w.Write(firstBytes)
	}))
	defer server.Close()
	defer releaseDownload()

	target := pm.MakePlugin("blocked-target", "repo")
	target.Package = server.URL
	firstResult := make(chan error, 1)
	go func() { firstResult <- pm.InstallPlugin(target, server.URL) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first download did not reach the server")
	}

	unrelatedResult := make(chan error, 1)
	go func() {
		unrelatedResult <- pm.InstallPluginFromURL("file://" + filepath.ToSlash(unrelatedArchive))
	}()
	select {
	case err := <-unrelatedResult:
		if err != nil {
			t.Fatalf("unrelated install failed while download was blocked: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked download prevented an unrelated plugin install")
	}

	attempting := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(attempting)
		secondResult <- pm.InstallPlugin(target, server.URL)
	}()
	<-attempting
	select {
	case err := <-secondResult:
		if !errors.Is(err, ErrPluginLifecycleBusy) {
			t.Fatalf("concurrent same-FQID install error = %v, want busy", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("same-FQID install neither rejected nor completed while lifecycle was active")
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("HTTP requests while first install was blocked = %d, want 1", got)
	}
	releaseDownload()
	if err := <-firstResult; err != nil {
		t.Fatalf("first update failed: %v", err)
	}
	if target.InstVer != "1.0" || !target.Loaded {
		t.Fatalf("first install left version %q loaded=%v, want 1.0 loaded",
			target.InstVer, target.Loaded)
	}
}

func TestLifecycleDuringECMAScriptInstallDefersUnloadAndAllowsOtherPlugin(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	targetArchive := filepath.Join(dataDir, "target-ecmascript.zip")
	otherArchive := filepath.Join(dataDir, "other-views.zip")
	writeRegressionPlugin(t, targetArchive, "lifecycle-target", "ecmascript", "1.0", false)
	writeRegressionPlugin(t, otherArchive, "lifecycle-other", "views", "1.0", false)
	target := pm.MakePlugin("lifecycle-target", "repo")
	target.Package = "file://" + filepath.ToSlash(targetArchive)
	otherURL := "file://" + filepath.ToSlash(otherArchive)

	loading := make(chan struct{})
	releaseLoad := make(chan struct{})
	reentrantResult := make(chan error, 1)
	pm.beforeESPluginLoad = func(pl *Plugin) {
		if pl != target {
			return
		}
		close(loading)
		reentrantResult <- pm.InstallPlugin(pl, pl.Package)
		<-releaseLoad
	}

	targetResult := make(chan error, 1)
	go func() { targetResult <- pm.InstallPlugin(target, target.Package) }()
	select {
	case <-loading:
	case <-time.After(3 * time.Second):
		t.Fatal("target ECMAScript load did not reach the barrier")
	}
	if err := <-reentrantResult; !errors.Is(err, ErrPluginLifecycleBusy) {
		t.Fatalf("reentrant same-FQID install = %v, want busy error", err)
	}
	if err := pm.RemovePlugin(target); !errors.Is(err, ErrPluginLifecycleBusy) {
		t.Fatalf("remove during active same-FQID lifecycle = %v, want busy error", err)
	}
	if err := pm.LoadInstalledPlugins(dataDir); !errors.Is(err, ErrPluginLifecycleBusy) {
		t.Fatalf("startup load during active plugin lifecycle = %v, want busy error", err)
	}

	pm.UnloadPlugin(target)
	otherResult := make(chan error, 1)
	go func() { otherResult <- pm.InstallPluginFromURL(otherURL) }()
	otherCompletedDuringTargetLoad := false
	select {
	case err := <-otherResult:
		otherCompletedDuringTargetLoad = true
		if err != nil {
			t.Fatalf("other-plugin install failed during target load: %v", err)
		}
	case <-time.After(3 * time.Second):
	}

	close(releaseLoad)
	if err := <-targetResult; err != nil {
		t.Fatalf("target install failed: %v", err)
	}
	if target.Loaded {
		t.Fatal("unload requested during ECMAScript initialization was lost")
	}
	if !target.Installed || target.InstVer != "1.0" {
		t.Fatalf("installed target state = %#v, want installed v1.0 but unloaded", target)
	}
	if !otherCompletedDuringTargetLoad {
		if err := <-otherResult; err != nil {
			t.Fatalf("other-plugin install failed after target load: %v", err)
		}
		t.Fatal("different-FQID install waited for ECMAScript initialization to finish")
	}
	if other := pm.FindPlugin("lifecycle-other@local"); other == nil || !other.Loaded {
		t.Fatalf("different-FQID plugin was not loaded: %#v", other)
	}
}

func TestDevPluginReloadDoesNotUnloadDuringActiveInstall(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	t.Chdir(dataDir)
	const pluginID = "reload-overlap"
	devPlugin := pluginID + "@dev"
	if err := os.Mkdir(devPlugin, 0755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(PluginControl{ID: pluginID, Type: "views", Version: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devPlugin, "plugin.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	if err := pm.LoadPlugin(devPlugin, "dev", 0); err != nil {
		t.Fatalf("load development plugin: %v", err)
	}
	target := pm.FindPlugin(devPlugin)
	if target == nil || !target.Loaded {
		t.Fatalf("development plugin state = %#v, want loaded plugin", target)
	}
	pm.devPlugins = []string{devPlugin}
	pm.SetTraceSystem(trace.NewTraceSystem(nil))
	unloads := 0
	target.Unload = func(*Plugin) { unloads++ }

	newArchive := filepath.Join(dataDir, "replacement.zip")
	writeRegressionPlugin(t, newArchive, pluginID, "views", "2.0", false)
	archiveBytes, err := os.ReadFile(newArchive)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDownload := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write(archiveBytes)
	}))
	defer server.Close()
	defer releaseDownload()

	installResult := make(chan error, 1)
	go func() { installResult <- pm.InstallPlugin(target, server.URL) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("install did not reach blocked download")
	}
	if err := ReloadDevPlugin(context.Background(), pm); err != nil {
		t.Fatalf("reload development plugin: %v", err)
	}
	if unloads != 0 {
		t.Fatalf("reload unloaded plugin during active installation %d times, want 0", unloads)
	}
	releaseDownload()
	if err := <-installResult; err != nil {
		t.Fatalf("replacement install: %v", err)
	}
	if unloads != 1 {
		t.Fatalf("install unload callback count = %d, want exactly one", unloads)
	}
	if !target.Loaded || target.InstVer != "2.0" {
		t.Fatalf("post-install state = loaded %v version %q, want loaded v2.0",
			target.Loaded, target.InstVer)
	}
}

func TestLoadRejectedDuringStartupDoesNotCreatePlugin(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	archive := filepath.Join(dataDir, "rejected-plugin.zip")
	writeRegressionPlugin(t, archive, "rejected-plugin", "views", "1.0", false)
	pluginURL, err := resolveZipPath(archive)
	if err != nil {
		t.Fatal(err)
	}
	pm.mu.Lock()
	before := len(pm.plugins)
	pm.startupLoading = true
	pm.mu.Unlock()
	defer func() {
		pm.mu.Lock()
		pm.startupLoading = false
		pm.mu.Unlock()
	}()

	err = pm.LoadPlugin(pluginURL, "repo", 0)
	if !errors.Is(err, ErrPluginLifecycleBusy) {
		t.Fatalf("load during startup = %v, want busy error", err)
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if got := len(pm.plugins); got != before {
		t.Fatalf("plugin count after rejected load = %d, want %d", got, before)
	}
	for _, pl := range pm.plugins {
		if pl.FQID == "rejected-plugin@repo" {
			t.Fatalf("rejected load left plugin state: %#v", pl)
		}
	}
}

func TestUnloadCallbackPanicRelocksAndReleasesLifecycle(t *testing.T) {
	pm, _ := newArchiveRegressionPluginManager(t)
	target := pm.MakePlugin("panic-unload", "repo")
	pm.mu.Lock()
	target.Loaded = true
	target.lifecycleActive = true
	target.unloadRequested = true
	target.Unload = func(*Plugin) { panic("expected unload panic") }

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		pm.finishPluginLifecycleLocked(target)
	}()
	if recovered == nil {
		t.Fatal("unload callback panic was swallowed")
	}
	if pm.mu.TryLock() {
		pm.mu.Unlock()
		t.Fatal("unload callback panic left pm.mu unlocked")
	}
	if target.lifecycleActive {
		t.Fatal("lifecycle gate remained active after callback panic")
	}
	if !target.Loaded {
		t.Fatal("plugin marked unloaded although unload callback panicked")
	}
	if target.Unload == nil {
		t.Fatal("unload callback was discarded after panic")
	}
	pm.mu.Unlock()
}
