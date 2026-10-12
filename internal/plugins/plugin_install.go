package plugins

// Plugin Installation and Removal - Plugin install/uninstall functionality
// This file contains functions for installing and removing plugins

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	fileaccesscore "github.com/czz/movian-go/internal/fileaccess"
)

// InstallPlugin installs a plugin
// C: plugin_install (plugins.c).
func (pm *PluginManager) InstallPlugin(pl *Plugin, packageURL string) error {
	pm.mu.Lock()

	// C: plugin_install's usage_event (plugins.c:1518-1520) precedes the fallback;
	// "source" is File only when a package path was passed explicitly.
	source := "Repo"
	if packageURL != "" {
		source = "File"
	}
	event := "Plugin install"
	if pl.CanUpgrade {
		event = "Plugin upgrade"
	}
	pm.usage.Event(event, 1, "plugin", pl.FQID, "source", source)

	// Use plugin's package URL if not provided
	if packageURL == "" {
		packageURL = pl.Package
		if packageURL == "" {
			pm.mu.Unlock()
			return fmt.Errorf("No package URL specified")
		}
	}
	pm.mu.Unlock()

	// C: plugin_install releases plugin_mutex around fa_load
	// (plugins.c:1540-1544). Go: keep fileaccess I/O outside pm.mu,
	// then serialize metadata, stored archive, and runtime loading together.
	tempPath, err := pm.downloadPlugin(packageURL)
	if err != nil {
		return fmt.Errorf("Failed to download plugin: %w", err)
	}
	defer os.Remove(tempPath)

	return pm.installPluginFromDownloadedArchive(pl, tempPath)
}

// Go extension: replacement rollback preserves the metadata and runtime flags
// that this installer changes; the previous archive is backed up separately.
type pluginInstallState struct {
	selectedViews    []pluginSelectedView
	packageURL       string
	title            string
	installedVersion string
	installed        bool
	loaded           bool
}


type pluginSelectedView struct {
	uiType   string
	class    string
	filename string
}

// Go extension: preserve selected plugin-owned views across failed replacements.
// Save stable identities, not property pointers that unloading destroys.
func (pm *PluginManager) snapshotSelectedPluginViewsLocked(pl *Plugin) []pluginSelectedView {
	var selected []pluginSelectedView
	for _, entry := range pl.Views {
		if entry.View == nil || entry.TypeProp == nil || entry.SettingProp == nil {
			continue
		}
		typeParent := entry.TypeProp.GetParent()
		settingParent := entry.SettingProp.GetParent()
		typeSelected := typeParent != nil && typeParent.GetSelectedChild() == entry.TypeProp
		settingSelected := settingParent != nil && settingParent.GetSelectedChild() == entry.SettingProp
		if typeSelected || settingSelected {
			selected = append(selected, pluginSelectedView{
				uiType: entry.View.UIType, class: entry.View.Class, filename: entry.Filename,
			})
		}
	}
	return selected
}

// Caller holds pm.mu. Select the recreated setting option, rather than invoking
// SelectView (which takes pm.mu again), so the persisted selection is updated.
func (pm *PluginManager) restoreSelectedPluginViewsLocked(pl *Plugin, selected []pluginSelectedView) {
	for _, old := range selected {
		for _, entry := range pl.Views {
			if entry.View != nil && entry.SettingProp != nil &&
				entry.View.UIType == old.uiType &&
				entry.View.Class == old.class && entry.Filename == old.filename {
				pm.propManager.Select(entry.SettingProp)
				break
			}
		}
	}
}

func snapshotPluginInstallState(pl *Plugin) pluginInstallState {
	return pluginInstallState{
		packageURL:       pl.Package,
		title:            pl.Title,
		installedVersion: pl.InstVer,
		installed:        pl.Installed,
		loaded:           pl.Loaded,
	}
}

func (pm *PluginManager) restorePluginInstallStateLocked(pl *Plugin, state pluginInstallState) {
	pl.Package = state.packageURL
	pl.Title = state.title
	pl.InstVer = state.installedVersion
	pl.Installed = state.installed
	pl.Loaded = state.loaded
	pm.UpdateState(pl)
}

// InstallPluginFromURL downloads, registers, installs, and loads a plugin ZIP.
// Go extension: native package opening and HTTP ZIP URLs share this entry point.
func (pm *PluginManager) InstallPluginFromURL(packageURL string) error {
	if packageURL == "" {
		return fmt.Errorf("No package URL specified")
	}
	tempPath, err := pm.downloadPlugin(packageURL)
	if err != nil {
		return fmt.Errorf("Failed to download plugin: %w", err)
	}
	defer os.Remove(tempPath)

	return pm.installPluginFromArchive(tempPath, packageURL)
}

// InstallPluginFromArchive installs and loads a ZIP supplied by the caller.
// Go extension: HTTP uploads supply a reader; the caller retains its ownership.
func (pm *PluginManager) InstallPluginFromArchive(archive io.Reader) error {
	tempFile, err := pm.createPluginArchiveFile()
	if err != nil {
		return fmt.Errorf("Failed to create plugin archive: %w", err)
	}
	defer os.Remove(tempFile.Name())
	if _, err := io.Copy(tempFile, archive); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("Failed to read plugin archive: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("Failed to close plugin archive: %w", err)
	}
	return pm.installPluginFromArchive(tempFile.Name(), "")
}

func (pm *PluginManager) installPluginFromArchive(tempPath, packageURL string) error {
	ctrl, err := pm.readPluginControlFromZip(tempPath)
	if err != nil {
		return fmt.Errorf("Failed to validate plugin archive: %w", err)
	}

	pm.installMu.Lock()
	defer pm.installMu.Unlock()

	pm.mu.Lock()
	defer pm.mu.Unlock()
	pl := pm.makePluginLocked(ctrl.ID, "local")
	previous := snapshotPluginInstallState(pl)
	pm.setPluginFromZipLocked(pl, ctrl, packageURL)
	return pm.installPluginFromDownloadedArchiveLocked(pl, tempPath, previous)
}

func (pm *PluginManager) installPluginFromDownloadedArchive(pl *Plugin, tempPath string) error {
	ctrl, err := pm.readPluginControlFromZip(tempPath)
	if err != nil {
		return fmt.Errorf("Failed to validate plugin archive: %w", err)
	}
	if ctrl.ID != pluginIDFromFQID(pl.FQID) {
		return fmt.Errorf("Plugin ID %q does not match target %q", ctrl.ID, pl.FQID)
	}

	pm.installMu.Lock()
	defer pm.installMu.Unlock()

	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.installPluginFromDownloadedArchiveLocked(pl, tempPath, snapshotPluginInstallState(pl))
}

// C: plugin_install (plugins.c) unloads, writes installed/<fqid>.zip, then loads
// with installed flags. Go extension: pm.mu also covers archive backup/rollback
// so failed replacements can restore the previous archive and loaded runtime.
func (pm *PluginManager) installPluginFromDownloadedArchiveLocked(pl *Plugin, tempPath string, previous pluginInstallState) error {
	if _, err := resolveZipPath(tempPath); err != nil {
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to resolve zip path: %w", err)
	}

	installDir := filepath.Join(getPersistentPath(), pm.storagePrefix, "installed")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to install plugin: %w", err)
	}
	installedPath := filepath.Join(installDir, fmt.Sprintf("%s.zip", pl.FQID))

	backupDir := ""
	backupPath := ""
	if _, err := os.Stat(installedPath); err == nil {
		backupDir, err = os.MkdirTemp(installDir, ".plugin-backup-*")
		if err != nil {
			pm.restorePluginInstallStateLocked(pl, previous)
			return fmt.Errorf("Failed to preserve installed plugin: %w", err)
		}
		backupPath = filepath.Join(backupDir, filepath.Base(installedPath))
		if err := os.Rename(installedPath, backupPath); err != nil {
			_ = os.RemoveAll(backupDir)
			pm.restorePluginInstallStateLocked(pl, previous)
			return fmt.Errorf("Failed to preserve installed plugin: %w", err)
		}
	} else if !os.IsNotExist(err) {
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to inspect installed plugin: %w", err)
	}
	hadBackup := backupPath != ""

	previous.selectedViews = pm.snapshotSelectedPluginViewsLocked(pl)
	pm.unloadPluginInternal(pl)
	installedArchivePath, err := pm.installToStorage(pl, tempPath)
	if err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, backupDir, backupPath, hadBackup,
			fmt.Errorf("Failed to install plugin: %w", err))
	}
	installedPath = installedArchivePath

	// C: #ifdef STOS — arch_sync_path(path) after fa_write/fa_close
	// (plugins.c:1610-1612). Go: mgosSyncPath is syncfs under -tags mgos.
	mgosSyncPath(installedPath)

	// C: plugin_resolve_zip_path(path) (plugins.c:1614)
	zipPath, err := resolveZipPath(installedPath)
	if err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, backupDir, backupPath, hadBackup,
			fmt.Errorf("Failed to resolve zip path: %w", err))
	}

	// C: plugin_load(zippath, ..., PLUGIN_LOAD_FORCE |
	// PLUGIN_LOAD_AS_INSTALLED | PLUGIN_LOAD_BY_USER) (plugins.c:1623-1625) —
	// AS_INSTALLED runs plugin_prop_setup + pl_installed=1 +
	// autoplugin_set_installed + update_state inside plugin_load.
	if err := pm.loadPluginLocked(zipPath, pl.Origin,
		PluginLoadForce|PluginLoadAsInstalled|PluginLoadByUser); err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, backupDir, backupPath, hadBackup,
			fmt.Errorf("Failed to load plugin: %w", err))
	}

	if backupDir != "" {
		_ = os.RemoveAll(backupDir)
	}
	pm.saveReposLocked()
	return nil
}

// Go extension: restore both the persisted package and its runtime after failure;
// if restoration fails, report it rather than claiming the old plugin is loaded.
func (pm *PluginManager) rollbackPluginInstallLocked(pl *Plugin, previous pluginInstallState, installedPath, backupDir, backupPath string, hadBackup bool, cause error) error {
	pm.unloadPluginInternal(pl)

	var rollbackErr error
	if err := os.Remove(installedPath); err != nil && !os.IsNotExist(err) {
		rollbackErr = err
	}
	archiveRestored := !hadBackup
	if hadBackup {
		if err := os.Rename(backupPath, installedPath); err != nil {
			if rollbackErr == nil {
				rollbackErr = err
			}
		} else {
			archiveRestored = true
			if err := os.RemoveAll(backupDir); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
		}
	}

	pm.restorePluginInstallStateLocked(pl, previous)
	if previous.loaded {
		pl.Loaded = false
		pm.UpdateState(pl)
		if hadBackup && archiveRestored {
			zipPath, err := resolveZipPath(installedPath)
			if err == nil {
				flags := PluginLoadForce
				if previous.installed {
					flags |= PluginLoadAsInstalled
				}
				err = pm.loadPluginLocked(zipPath, pl.Origin, flags)
			}
			if err != nil {
				pm.unloadPluginInternal(pl)
				pm.restorePluginInstallStateLocked(pl, previous)
				pl.Loaded = false
				pm.UpdateState(pl)
				if rollbackErr == nil {
					rollbackErr = fmt.Errorf("failed to reload previous plugin: %w", err)
				}
			} else {
				pm.restoreSelectedPluginViewsLocked(pl, previous.selectedViews)
			}
		} else if rollbackErr == nil {
			rollbackErr = fmt.Errorf("previous installed plugin archive is unavailable")
		}
	}

	if rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %w)", cause, rollbackErr)
	}
	return cause
}

// RemovePlugin removes an installed plugin
func (pm *PluginManager) RemovePlugin(pl *Plugin) error {
	pm.installMu.Lock()
	defer pm.installMu.Unlock()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pl.Installed {
		return fmt.Errorf("Plugin is not installed")
	}

	// C: plugins.c:1486 — usage_event("Plugin remove", plugin=fqid)
	pm.usage.Event("Plugin remove", 1, "plugin", pl.FQID)

	// Unload plugin
	pm.unloadPluginInternal(pl)

	// Remove from storage
	if err := pm.removeFromStorage(pl); err != nil {
		return fmt.Errorf("Failed to remove plugin from storage: %w", err)
	}

	// Mark as not installed
	pl.Installed = false
	pl.InstVer = ""
	pl.Loaded = false
	pm.autoPluginSetInstalledLocked(pl.FQID, false)
	pm.UpdateState(pl)

	// Save repos configuration
	pm.saveReposLocked()
	return nil
}

func (pm *PluginManager) createPluginArchiveFile() (*os.File, error) {
	// os.TempDir() may not exist on Android; use the configured cache directory.
	dir := pm.gcfg().CachePath
	if dir == "" {
		dir = getPersistentPath()
	}
	return os.CreateTemp(dir, "plugin-*.zip")
}

// maxPluginDownloadBytes caps untrusted remote packages independent of the HTTP upload limit.
const maxPluginDownloadBytes int64 = 64 << 20

// copyPluginArchiveBounded reads at most limit+1 bytes to detect oversized
// packages without copying an unbounded stream into the cache.
func copyPluginArchiveBounded(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("plugin ZIP exceeds %d MiB download limit", limit>>20)
	}
	return n, nil
}

// downloadPlugin copies a package through fileaccess.
// C: plugin_install calls fa_load (plugins.c:1542) for registered URL protocols.
// Go: stream into a cache file instead of keeping the whole ZIP in memory;
// local updater packages and HTTP URLs still use the same fileaccess protocols.
func (pm *PluginManager) downloadPlugin(packageURL string) (string, error) {
	tempFile, err := pm.createPluginArchiveFile()
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tempFile.Name())
		}
	}()
	defer tempFile.Close()

	handle, err := fileaccesscore.Open(pm.faManager, packageURL)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer handle.Close()

	if _, err := copyPluginArchiveBounded(tempFile, handle, maxPluginDownloadBytes); err != nil {
		return "", fmt.Errorf("write failed: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close failed: %w", err)
	}
	success = true
	return tempFile.Name(), nil
}

// installToStorage installs a plugin to persistent storage
func (pm *PluginManager) installToStorage(pl *Plugin, sourcePath string) (string, error) {
	// Create install directory
	persistentPath := getPersistentPath()
	installDir := filepath.Join(persistentPath, pm.storagePrefix, "installed")

	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}

	// Create destination filename
	destFilename := fmt.Sprintf("%s.zip", pl.FQID)
	destPath := filepath.Join(installDir, destFilename)

	// Copy file
	if err := copyFile(sourcePath, destPath); err != nil {
		return "", err
	}

	return destPath, nil
}

// removeFromStorage removes a plugin from persistent storage
func (pm *PluginManager) removeFromStorage(pl *Plugin) error {
	persistentPath := getPersistentPath()
	installDir := filepath.Join(persistentPath, pm.storagePrefix, "installed")

	// Try multiple possible filenames
	possibleNames := []string{
		fmt.Sprintf("%s.zip", pl.FQID),
		fmt.Sprintf("%s@%s.zip", pl.FQID, pl.Origin),
	}

	for _, name := range possibleNames {
		path := filepath.Join(installDir, name)
		if err := os.Remove(path); err == nil {
			return nil
		}
	}

	return fmt.Errorf("Plugin file not found in storage")
}

// UpgradePlugin upgrades a plugin to the latest version
func (pm *PluginManager) UpgradePlugin(pl *Plugin) error {
	if !pl.CanUpgrade {
		return fmt.Errorf("Plugin cannot be upgraded")
	}

	// Install new version
	if err := pm.InstallPlugin(pl, ""); err != nil {
		return err
	}

	return nil
}

// UpgradeAllPlugins upgrades all upgradable plugins
func (pm *PluginManager) UpgradeAllPlugins() error {
	upgradable := pm.GetUpgradablePlugins()

	for _, pl := range upgradable {
		_ = pm.UpgradePlugin(pl)
	}

	return nil
}

// AutoUpgradePlugins performs automatic upgrade for plugins with auto-upgrade enabled
func (pm *PluginManager) AutoUpgradePlugins() {
	pm.mu.RLock()
	plugins := make([]*Plugin, len(pm.plugins))
	copy(plugins, pm.plugins)
	pm.mu.RUnlock()

	for _, pl := range plugins {
		if !pl.CanUpgrade || !pl.AutoUpgrade {
			continue
		}

		_ = pm.UpgradePlugin(pl)
	}

	pm.UpdateGlobalState()
}

// SetPluginAutoUpgrade sets the auto-upgrade flag for a plugin
func (pm *PluginManager) SetPluginAutoUpgrade(fqid string, autoUpgrade bool) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var pl *Plugin
	for _, p := range pm.plugins {
		if p.FQID == fqid {
			pl = p
			break
		}
	}
	if pl == nil {
		return fmt.Errorf("Plugin not found: %s", fqid)
	}

	pl.AutoUpgrade = autoUpgrade
	return nil
}

// C: plugin_open_file (plugins.c) resolves the ZIP and parses plugin.json before
// plugin_make(id, "local"). Go: share that validation with reader-based uploads.
func (pm *PluginManager) readPluginControlFromZip(zipfile string) (PluginControl, error) {
	zipPath, err := resolveZipPath(zipfile)
	if err != nil {
		return PluginControl{}, fmt.Errorf("Unable to open %s -- Not a valid plugin archive", zipfile)
	}

	pluginJSONPath := fmt.Sprintf("%s/plugin.json", zipPath)
	data, err := loadFileWithFA(pluginJSONPath, pm.faManager)
	if err != nil {
		return PluginControl{}, fmt.Errorf("Unable to open %s -- %w", pluginJSONPath, err)
	}

	var ctrl PluginControl
	if err := json.Unmarshal(data, &ctrl); err != nil {
		return PluginControl{}, fmt.Errorf("Unable to parse plugin.json: %w", err)
	}
	if ctrl.ID == "" {
		return PluginControl{}, fmt.Errorf("Missing plugin ID")
	}
	// Go extension: FQID uses <id>@<origin>; reject its delimiter and path
	// separators before using the ID in an installed archive filename.
	if strings.ContainsAny(ctrl.ID, `/\\@`) {
		return PluginControl{}, fmt.Errorf("Invalid plugin ID %q", ctrl.ID)
	}
	if ctrl.Type == "" {
		return PluginControl{}, fmt.Errorf("Missing plugin type")
	}
	return ctrl, nil
}

func pluginIDFromFQID(fqid string) string {
	id, _, ok := strings.Cut(fqid, "@")
	if !ok {
		return ""
	}
	return id
}

func (pm *PluginManager) setPluginFromZipLocked(pl *Plugin, ctrl PluginControl, packageURL string) {
	if ctrl.Title != "" {
		pl.Title = ctrl.Title
	}
	pl.InstVer = ctrl.Version
	pl.Package = packageURL
	pm.UpdateState(pl)
}

// ProbeForAutoInstall probes a file for auto-installable plugins
func (pm *PluginManager) ProbeForAutoInstall(fh any, buf []byte, len int, url string) bool {
	// Check if the file is a plugin and if it should be auto-installed
	// ZIP file signature check
	if len < 4 {
		return false
	}

	// ZIP file signature
	if buf[0] == 0x50 && buf[1] == 0x4B && buf[2] == 0x03 && buf[3] == 0x04 {
		return true
	}

	return false
}

// CheckPrefixForAutoInstall checks if a URL prefix should trigger auto-install
func (pm *PluginManager) CheckPrefixForAutoInstall(url string) bool {
	// Check if the URL prefix is configured for auto-install
	return false
}

// Helper functions

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

func getPersistentPath() string {
	if v := globalPersistentPath.Load(); v != nil {
		return v.(string)
	}
	return "/tmp/movian"
}

var globalPersistentPath atomic.Value // string — C: gconf.persistent_path

// Wired by cmd/movian-go init; nil-safe methods make unwired calls no-ops.

// SetPersistentPath sets the persistent storage path for plugins.
func (pm *PluginManager) SetPersistentPath(path string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.persistentPath = path
	globalPersistentPath.Store(path)
}

// InstallPluginByID installs a plugin by its FQID.
func (pm *PluginManager) InstallPluginByID(id string) error {
	pl := pm.FindPlugin(id)
	if pl == nil {
		return fmt.Errorf("Plugin not found: %s", id)
	}
	return pm.InstallPlugin(pl, "")
}

// UninstallPluginByID removes a plugin by its FQID.
func (pm *PluginManager) UninstallPluginByID(id string) error {
	pl := pm.FindPlugin(id)
	if pl == nil {
		return fmt.Errorf("Plugin not found: %s", id)
	}
	return pm.RemovePlugin(pl)
}
