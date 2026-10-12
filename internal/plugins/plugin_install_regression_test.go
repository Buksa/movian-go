package plugins

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileaccesscore "github.com/czz/movian-go/internal/fileaccess"
	"github.com/czz/movian-go/internal/gconf"
	propcore "github.com/czz/movian-go/internal/prop"
	settingscore "github.com/czz/movian-go/internal/settings"
)

func newArchiveRegressionPluginManager(t *testing.T) (*PluginManager, string) {
	t.Helper()
	dataDir := t.TempDir()
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

func TestStartupLoadingWaitsForInstallTransaction(t *testing.T) {
	pm, dataDir := newArchiveRegressionPluginManager(t)
	if err := os.MkdirAll(filepath.Join(dataDir, "mrp", "installed"), 0755); err != nil {
		t.Fatal(err)
	}
	pm.installMu.Lock()
	done := make(chan error, 1)
	go func() { done <- pm.LoadInstalledPlugins(dataDir) }()
	select {
	case err := <-done:
		t.Fatalf("startup loading bypassed an active install transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	pm.installMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup loading did not continue after the transaction completed")
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
