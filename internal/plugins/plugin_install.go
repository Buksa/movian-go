package plugins

// Plugin Installation and Removal - Plugin install/uninstall functionality
// This file contains functions for installing and removing plugins

import (
	"encoding/json"
	"errors"
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
// ErrPluginLifecycleBusy reports that another lifecycle transition already owns this FQID.
var ErrPluginLifecycleBusy = errors.New("plugin lifecycle operation already in progress")

func (pm *PluginManager) beginPluginLifecycleLocked(pl *Plugin, allowStartup bool) error {
	if pl.lifecycleActive || (pm.startupLoading && !allowStartup) {
		return fmt.Errorf("%w: %s", ErrPluginLifecycleBusy, pl.FQID)
	}
	pl.lifecycleActive = true
	return nil
}

func (pm *PluginManager) finishPluginLifecycleLocked(pl *Plugin) {
	defer func() {
		pl.lifecycleActive = false
		pl.unloadRequested = false
	}()
	if pl.unloadRequested {
		pl.unloadRequested = false
		if pl.Loaded {
			pm.unloadPluginInternal(pl)
			pl.Loaded = false
			pm.UpdateState(pl)
		}
	}
}

// InstallPlugin downloads outside pm.mu and commits under the per-FQID state gate.
func (pm *PluginManager) InstallPlugin(pl *Plugin, packageURL string) error {
	pm.mu.Lock()
	source := "Repo"
	if packageURL != "" {
		source = "File"
	} else {
		packageURL = pl.Package
	}
	if packageURL == "" {
		pm.mu.Unlock()
		return fmt.Errorf("No package URL specified")
	}
	if err := pm.beginPluginLifecycleLocked(pl, false); err != nil {
		pm.mu.Unlock()
		return err
	}
	event := "Plugin install"
	if pl.CanUpgrade {
		event = "Plugin upgrade"
	}
	pm.usage.Event(event, 1, "plugin", pl.FQID, "source", source)
	previous := snapshotPluginInstallState(pl)
	pm.mu.Unlock()
	defer func() {
		pm.mu.Lock()
		defer pm.mu.Unlock()
		pm.finishPluginLifecycleLocked(pl)
	}()

	tempPath, err := pm.downloadPlugin(packageURL)
	if err != nil {
		return fmt.Errorf("Failed to download plugin: %w", err)
	}
	defer os.Remove(tempPath)
	if previous.loaded && !previous.installed && previous.packageURL != "" &&
		previous.rollbackArchivePath == "" {
		previous.rollbackArchivePath, err = pm.downloadPluginWithPattern(
			previous.packageURL, "plugin-rollback-*.zip")
		if err != nil {
			return fmt.Errorf("Failed to prepare rollback plugin: %w", err)
		}
		defer func() {
			pm.mu.Lock()
			owned := pl.rollbackArchivePath
			pm.mu.Unlock()
			if previous.rollbackArchivePath != "" && previous.rollbackArchivePath != owned {
				_ = os.Remove(previous.rollbackArchivePath)
			}
		}()
	}

	return pm.installPluginFromDownloadedArchive(pl, tempPath, previous)
}

func (pm *PluginManager) findPluginLocked(fqid string) *Plugin {
	for _, candidate := range pm.plugins {
		if candidate.FQID == fqid {
			return candidate
		}
	}
	return nil
}

// Go extension: replacement rollback preserves the metadata and runtime flags
// that this installer changes; the previous archive is backed up separately.
type pluginInstallState struct {
	selectedViews       []pluginSelectedView
	packageURL          string
	title               string
	installedVersion    string
	installed           bool
	loaded              bool
	rollbackArchivePath string
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
		packageURL:          pl.Package,
		title:               pl.Title,
		installedVersion:    pl.InstVer,
		installed:           pl.Installed,
		loaded:              pl.Loaded,
		rollbackArchivePath: pl.rollbackArchivePath,
	}
}

func (pm *PluginManager) restorePluginInstallStateLocked(pl *Plugin, state pluginInstallState) {
	pl.Package = state.packageURL
	pl.Title = state.title
	pl.InstVer = state.installedVersion
	pl.Installed = state.installed
	pl.Loaded = state.loaded
	pl.rollbackArchivePath = state.rollbackArchivePath
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
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pl := pm.makePluginLocked(ctrl.ID, "local")
	if err := pm.beginPluginLifecycleLocked(pl, false); err != nil {
		return err
	}
	defer pm.finishPluginLifecycleLocked(pl)
	previous := snapshotPluginInstallState(pl)
	pm.setPluginFromZipLocked(pl, ctrl, packageURL)
	return pm.installPluginFromDownloadedArchiveLocked(pl, tempPath, previous)
}

func (pm *PluginManager) installPluginFromDownloadedArchive(pl *Plugin, tempPath string, previous pluginInstallState) error {
	ctrl, err := pm.readPluginControlFromZip(tempPath)
	if err != nil {
		return fmt.Errorf("Failed to validate plugin archive: %w", err)
	}
	if ctrl.ID != pluginIDFromFQID(pl.FQID) {
		return fmt.Errorf("Plugin ID %q does not match target %q", ctrl.ID, pl.FQID)
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.findPluginLocked(pl.FQID) != pl ||
		pl.Package != previous.packageURL || pl.Loaded != previous.loaded ||
		pl.Installed != previous.installed {
		return fmt.Errorf("Plugin %q changed while preparing installation", pl.FQID)
	}
	return pm.installPluginFromDownloadedArchiveLocked(pl, tempPath, previous)
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
	_, statErr := os.Stat(installedPath)
	hadBackup := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to inspect installed plugin: %w", statErr)
	}

	txnDir, err := createPluginInstallTransaction(installDir, filepath.Base(installedPath), hadBackup)
	if err != nil {
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to prepare plugin installation: %w", err)
	}
	backupPath := filepath.Join(txnDir, "backup.zip")
	if err := writePluginInstallPhase(txnDir, pluginInstallReplacing); err != nil {
		_ = pm.removeInstallDirectory(txnDir)
		pm.restorePluginInstallStateLocked(pl, previous)
		return fmt.Errorf("Failed to persist plugin replacement state: %w", err)
	}
	if hadBackup {
		if err := os.Rename(installedPath, backupPath); err != nil {
			_ = pm.removeInstallDirectory(txnDir)
			pm.restorePluginInstallStateLocked(pl, previous)
			return fmt.Errorf("Failed to preserve installed plugin: %w", err)
		}
		if err := syncInstallDirectory(installDir); err != nil {
			return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, true,
				fmt.Errorf("sync plugin backup: %w", err))
		}
	}

	previous.selectedViews = pm.snapshotSelectedPluginViewsLocked(pl)
	pl.rollbackArchivePath = ""
	pm.unloadPluginInternal(pl)
	installedArchivePath, err := pm.installToStorage(pl, tempPath, txnDir)
	if err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, hadBackup,
			fmt.Errorf("Failed to install plugin: %w", err))
	}
	installedPath = installedArchivePath
	mgosSyncPath(installedPath)
	if err := syncInstallDirectory(installDir); err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, hadBackup,
			fmt.Errorf("sync installed plugin archive: %w", err))
	}

	zipPath, err := resolveZipPath(installedPath)
	if err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, hadBackup,
			fmt.Errorf("Failed to resolve zip path: %w", err))
	}
	if err := pm.loadPluginLockedMode(zipPath, pl.Origin,
		PluginLoadForce|PluginLoadAsInstalled|PluginLoadByUser, true, false); err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, hadBackup,
			fmt.Errorf("Failed to load plugin: %w", err))
	}

	pm.restoreSelectedPluginViewsLocked(pl, previous.selectedViews)
	if err := writePluginInstallPhase(txnDir, pluginInstallCommitted); err != nil {
		return pm.rollbackPluginInstallLocked(pl, previous, installedPath, txnDir, backupPath, hadBackup,
			fmt.Errorf("Failed to persist plugin installation commit: %w", err))
	}
	pm.saveReposLocked()

	pl.rollbackArchivePath = previous.rollbackArchivePath
	pm.cleanupRollbackArchiveLocked(pl)
	if err := pm.removeInstallDirectory(txnDir); err != nil {
		if pm.ts != nil {
			pm.ts.Error("plugins", "Plugin installation committed; cleanup of %s is pending: %v\n",
				txnDir, err)
		}
		return nil
	}
	if err := syncInstallDirectory(installDir); err != nil && pm.ts != nil {
		pm.ts.Error("plugins", "Plugin installation committed; sync of %s failed: %v\n",
			installDir, err)
	}
	return nil
}

// Go extension: restore both the persisted package and its runtime after failure;
// if restoration fails, report it rather than claiming the old plugin is loaded.
func (pm *PluginManager) rollbackPluginInstallLocked(pl *Plugin, previous pluginInstallState, installedPath, txnDir, backupPath string, hadBackup bool, cause error) error {
	if err := writePluginInstallPhase(txnDir, pluginInstallReplacing); err != nil {
		if cause == nil {
			return fmt.Errorf("rollback state could not be persisted: %w", err)
		}
		return fmt.Errorf("%w (rollback state could not be persisted: %w)", cause, err)
	}
	pm.unloadPluginInternal(pl)

	var rollbackErr error
	if err := os.Remove(installedPath); err != nil && !os.IsNotExist(err) {
		rollbackErr = err
	}
	archiveRestored := !hadBackup && rollbackErr == nil
	if hadBackup && rollbackErr == nil {
		if err := os.Rename(backupPath, installedPath); err != nil {
			rollbackErr = err
		} else {
			archiveRestored = true
		}
	}
	if rollbackErr == nil {
		installDir := filepath.Dir(installedPath)
		if err := syncInstallDirectory(installDir); err != nil {
			rollbackErr = err
		} else if err := pm.removeInstallDirectory(txnDir); err != nil {
			rollbackErr = err
		} else if err := syncInstallDirectory(installDir); err != nil {
			rollbackErr = err
		}
	}

	pm.restorePluginInstallStateLocked(pl, previous)
	if previous.loaded {
		pl.Loaded = false
		pm.UpdateState(pl)
		var zipPath string
		var err error
		var rollbackArchivePath string
		if hadBackup && archiveRestored {
			zipPath, err = resolveZipPath(installedPath)
		} else if !hadBackup && previous.packageURL != "" {
			rollbackArchivePath = previous.rollbackArchivePath
			if rollbackArchivePath != "" {
				if _, statErr := os.Stat(rollbackArchivePath); os.IsNotExist(statErr) {
					rollbackArchivePath = ""
				} else if statErr != nil {
					err = statErr
				}
			}
			if rollbackArchivePath == "" {
				err = fmt.Errorf("previous rollback archive was not prepared")
			}
			if err == nil {
				var ctrl PluginControl
				ctrl, err = pm.readPluginControlFromZip(rollbackArchivePath)
				if err == nil && ctrl.ID != pluginIDFromFQID(pl.FQID) {
					err = fmt.Errorf("previous package ID %q does not match plugin %q",
						ctrl.ID, pl.FQID)
				}
				if err == nil {
					zipPath, err = resolveZipPath(rollbackArchivePath)
				}
			}
		} else {
			err = fmt.Errorf("previous installed plugin archive is unavailable")
		}
		pl.rollbackArchivePath = ""
		if err == nil {
			flags := PluginLoadForce
			if previous.installed {
				flags |= PluginLoadAsInstalled
			}
			err = pm.loadPluginLockedMode(zipPath, pl.Origin, flags, true, false)
		}
		if err != nil {
			pm.unloadPluginInternal(pl)
			pm.restorePluginInstallStateLocked(pl, previous)
			pl.Loaded = false
			pm.UpdateState(pl)
			if rollbackArchivePath != "" &&
				rollbackArchivePath != previous.rollbackArchivePath {
				if removeErr := os.Remove(rollbackArchivePath); removeErr != nil &&
					!os.IsNotExist(removeErr) && rollbackErr == nil {
					rollbackErr = fmt.Errorf("remove failed rollback cache: %w", removeErr)
				}
			}
			if rollbackErr == nil {
				rollbackErr = fmt.Errorf("failed to reload previous plugin: %w", err)
			}
		} else {
			pm.restoreSelectedPluginViewsLocked(pl, previous.selectedViews)
			if !hadBackup && !previous.installed {
				pl.rollbackArchivePath = rollbackArchivePath
			} else if previous.rollbackArchivePath != "" {
				pl.rollbackArchivePath = previous.rollbackArchivePath
				pm.cleanupRollbackArchiveLocked(pl)
			}
		}
	}

	if rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %w)", cause, rollbackErr)
	}
	return cause
}

// RemovePlugin removes an installed plugin.
func (pm *PluginManager) RemovePlugin(pl *Plugin) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if err := pm.beginPluginLifecycleLocked(pl, false); err != nil {
		return err
	}
	defer pm.finishPluginLifecycleLocked(pl)
	if !pl.Installed {
		return fmt.Errorf("Plugin is not installed")
	}
	pm.usage.Event("Plugin remove", 1, "plugin", pl.FQID)
	pm.unloadPluginInternal(pl)
	if err := pm.removeFromStorage(pl); err != nil {
		return fmt.Errorf("Failed to remove plugin from storage: %w", err)
	}
	pl.Installed = false
	pl.InstVer = ""
	pl.Loaded = false
	pm.autoPluginSetInstalledLocked(pl.FQID, false)
	pm.UpdateState(pl)
	pm.saveReposLocked()
	return nil
}

func (pm *PluginManager) removeInstallDirectory(path string) error {
	if pm.removeInstallDir != nil {
		return pm.removeInstallDir(path)
	}
	return os.RemoveAll(path)
}

func (pm *PluginManager) createPluginArchiveFile() (*os.File, error) {
	return pm.createPluginArchiveFileWithPattern("plugin-*.zip")
}

func (pm *PluginManager) createPluginArchiveFileWithPattern(pattern string) (*os.File, error) {
	// os.TempDir() may not exist on Android; use the configured cache directory.
	dir := pm.gcfg().CachePath
	if dir == "" {
		dir = getPersistentPath()
	}
	return os.CreateTemp(dir, pattern)
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
	return pm.downloadPluginWithPattern(packageURL, "plugin-*.zip")
}

func (pm *PluginManager) downloadPluginWithPattern(packageURL, pattern string) (string, error) {
	tempFile, err := pm.createPluginArchiveFileWithPattern(pattern)
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

// installToStorage stages the validated archive inside its transaction directory
// and atomically renames it to the canonical path on the same filesystem.
func (pm *PluginManager) installToStorage(pl *Plugin, sourcePath, txnDir string) (string, error) {
	installDir := filepath.Join(getPersistentPath(), pm.storagePrefix, "installed")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}

	destPath := filepath.Join(installDir, fmt.Sprintf("%s.zip", pl.FQID))
	staged, err := os.CreateTemp(txnDir, ".staged-*.zip")
	if err != nil {
		return "", err
	}
	stagedPath := staged.Name()
	if err := staged.Close(); err != nil {
		_ = os.Remove(stagedPath)
		return "", err
	}
	defer os.Remove(stagedPath)
	if err := copyFile(sourcePath, stagedPath); err != nil {
		return "", err
	}
	if err := os.Chmod(stagedPath, 0644); err != nil {
		return "", err
	}
	file, err := os.OpenFile(stagedPath, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return "", syncErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	ctrl, err := pm.readPluginControlFromZip(stagedPath)
	if err != nil {
		return "", fmt.Errorf("staged plugin archive is invalid: %w", err)
	}
	if ctrl.ID != pluginIDFromArchiveFilename(filepath.Base(destPath)) {
		return "", fmt.Errorf("staged plugin ID %q does not match target %q",
			ctrl.ID, filepath.Base(destPath))
	}
	if err := os.Rename(stagedPath, destPath); err != nil {
		return "", err
	}
	if err := syncInstallDirectory(txnDir); err != nil {
		return "", err
	}
	return destPath, nil
}

func pluginIDFromArchiveFilename(filename string) string {
	name := strings.TrimSuffix(filepath.Base(filename), ".zip")
	if id, _, ok := strings.Cut(name, "@"); ok {
		return id
	}
	return name
}

// Go extension: recover the prior valid archive whenever an unfinished
// replacement leaves its backup directory behind. A successful replacement
// removes that directory only after the new plugin has loaded.
func (pm *PluginManager) recoverInterruptedPluginInstalls(installDir string) error {
	entries, err := os.ReadDir(installDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var recoveryErrors []error
	recordErr := func(err error) {
		recoveryErrors = append(recoveryErrors, err)
	}
	for _, entry := range entries {
		path := filepath.Join(installDir, entry.Name())
		if entry.IsDir() && strings.HasPrefix(entry.Name(), pluginInstallTransactionPrefix) {
			if err := pm.recoverPluginInstallTransaction(installDir, path); err != nil {
				metaBytes, _ := os.ReadFile(filepath.Join(path, pluginInstallTransactionMeta))
				var meta pluginInstallTransaction
				_ = json.Unmarshal(metaBytes, &meta)
				fqid := strings.TrimSuffix(meta.Target, ".zip")
				recordErr(&PluginRecoveryError{Path: path, FQID: fqid, Err: err})
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".plugin-install-") && !entry.IsDir() &&
			!isCanonicalPluginArchiveFilename(entry.Name()) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				recordErr(fmt.Errorf("remove stale plugin staging file %s: %w", path, err))
			}
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".plugin-backup-") {
			continue
		}
		backupEntries, err := os.ReadDir(path)
		if err != nil {
			recordErr(fmt.Errorf("inspect plugin backup %s: %w", path, err))
			continue
		}
		var backupPath string
		for _, backup := range backupEntries {
			if !backup.IsDir() && strings.HasSuffix(backup.Name(), ".zip") {
				backupPath = filepath.Join(path, backup.Name())
				break
			}
		}
		if backupPath == "" {
			if len(backupEntries) == 0 {
				if err := pm.removeInstallDirectory(path); err != nil {
					recordErr(fmt.Errorf("remove empty plugin backup %s: %w", path, err))
				}
			}
			continue
		}
		installedPath := filepath.Join(installDir, filepath.Base(backupPath))
		current, currentErr := pm.readPluginControlFromZip(installedPath)
		currentValid := currentErr == nil &&
			current.ID == pluginIDFromArchiveFilename(filepath.Base(installedPath))
		if currentValid {
			if err := pm.removeInstallDirectory(path); err != nil {
				recordErr(fmt.Errorf("remove stale plugin backup %s: %w", path, err))
			}
			continue
		}

		backup, err := pm.readPluginControlFromZip(backupPath)
		if err != nil || backup.ID != pluginIDFromArchiveFilename(filepath.Base(backupPath)) {
			if err == nil {
				err = fmt.Errorf("manifest ID %q does not match backup filename", backup.ID)
			}
			recordErr(fmt.Errorf("reject invalid plugin backup %s: %w", backupPath, err))
			continue
		}
		if err := os.Remove(installedPath); err != nil && !os.IsNotExist(err) {
			recordErr(fmt.Errorf("remove interrupted plugin archive %s: %w", installedPath, err))
			continue
		}
		if err := os.Rename(backupPath, installedPath); err != nil {
			recordErr(fmt.Errorf("restore plugin backup %s: %w", backupPath, err))
			continue
		}
		if err := syncInstallDirectory(installDir); err != nil {
			recordErr(fmt.Errorf("sync restored plugin archive %s: %w", installedPath, err))
			continue
		}
		if err := pm.removeInstallDirectory(path); err != nil {
			recordErr(fmt.Errorf("remove recovered plugin backup %s: %w", path, err))
		}
	}
	var combined error
	for _, recoveryErr := range recoveryErrors {
		combined = errorsJoin(combined, recoveryErr)
	}
	return combined
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
		return PluginControl{}, fmt.Errorf("Unable to open %s -- Not a valid plugin archive: %w",
			zipfile, err)
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
