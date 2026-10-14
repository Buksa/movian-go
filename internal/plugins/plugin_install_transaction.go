package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pluginInstallTransactionPrefix = ".plugin-txn-"
	pluginInstallTransactionPhase  = "phase"
	pluginInstallTransactionMeta   = "transaction.json"
	pluginInstallPrepared          = byte('P')
	pluginInstallReplacing         = byte('R')
	pluginInstallCommitted         = byte('C')
)

type pluginInstallTransaction struct {
	Version   int    `json:"version"`
	Target    string `json:"target"`
	HadBackup bool   `json:"had_backup"`
}

// PluginRecoveryError identifies the transaction or archive that failed recovery.
type PluginRecoveryError struct {
	Path string
	FQID string
	Err  error
}

func (e *PluginRecoveryError) Error() string {
	if e.FQID != "" {
		return fmt.Sprintf("recover plugin %s at %s: %v", e.FQID, e.Path, e.Err)
	}
	return fmt.Sprintf("recover plugin install at %s: %v", e.Path, e.Err)
}

func (e *PluginRecoveryError) Unwrap() error { return e.Err }

// createPluginInstallTransaction persists rollback identity before touching the
// installed archive. The phase file is created once and later updated in place,
// avoiding destination-replacement rename semantics on Windows.
func createPluginInstallTransaction(installDir, target string, hadBackup bool) (string, error) {
	txnDir, err := os.MkdirTemp(installDir, pluginInstallTransactionPrefix+"*")
	if err != nil {
		return "", err
	}
	cleanup := func(cause error) (string, error) {
		_ = os.RemoveAll(txnDir)
		return "", cause
	}
	meta := pluginInstallTransaction{Version: 1, Target: filepath.Base(target), HadBackup: hadBackup}
	if meta.Target != target || !strings.HasSuffix(meta.Target, ".zip") {
		return cleanup(fmt.Errorf("invalid plugin transaction target %q", target))
	}
	if _, _, ok := strings.Cut(strings.TrimSuffix(meta.Target, ".zip"), "@"); !ok {
		return cleanup(fmt.Errorf("plugin transaction target lacks FQID origin: %q", target))
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return cleanup(err)
	}
	if err := writeSyncedNewFile(filepath.Join(txnDir, pluginInstallTransactionMeta), data); err != nil {
		return cleanup(err)
	}
	if err := writeSyncedNewFile(filepath.Join(txnDir, pluginInstallTransactionPhase), []byte{pluginInstallPrepared}); err != nil {
		return cleanup(err)
	}
	if err := syncInstallDirectory(txnDir); err != nil {
		return cleanup(fmt.Errorf("sync plugin transaction directory: %w", err))
	}
	if err := syncInstallDirectory(installDir); err != nil {
		return cleanup(fmt.Errorf("sync installed directory: %w", err))
	}
	return txnDir, nil
}

func writeSyncedNewFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func writePluginInstallPhase(txnDir string, phase byte) error {
	file, err := os.OpenFile(filepath.Join(txnDir, pluginInstallTransactionPhase), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, writeErr := file.WriteAt([]byte{phase}, 0)
	if writeErr == nil && n != 1 {
		writeErr = fmt.Errorf("write plugin transaction phase: short write")
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (pm *PluginManager) recoverPluginInstallTransaction(installDir, txnDir string) error {
	metaBytes, err := os.ReadFile(filepath.Join(txnDir, pluginInstallTransactionMeta))
	if err != nil {
		return fmt.Errorf("read transaction metadata: %w", err)
	}
	var meta pluginInstallTransaction
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("decode transaction metadata: %w", err)
	}
	if meta.Version != 1 || filepath.Base(meta.Target) != meta.Target ||
		!strings.HasSuffix(meta.Target, ".zip") {
		return fmt.Errorf("invalid transaction metadata for %s", txnDir)
	}
	fqid := strings.TrimSuffix(meta.Target, ".zip")
	pluginID, _, ok := strings.Cut(fqid, "@")
	if !ok || pluginID == "" {
		return fmt.Errorf("invalid transaction FQID %q", fqid)
	}
	phaseBytes, err := os.ReadFile(filepath.Join(txnDir, pluginInstallTransactionPhase))
	if err != nil || len(phaseBytes) != 1 {
		if err == nil {
			err = fmt.Errorf("invalid transaction phase length %d", len(phaseBytes))
		}
		return fmt.Errorf("read transaction phase for %s: %w", fqid, err)
	}

	target := filepath.Join(installDir, meta.Target)
	backup := filepath.Join(txnDir, "backup.zip")
	phase := phaseBytes[0]
	if phase == pluginInstallPrepared {
		return pm.removeRecoveredTransaction(txnDir)
	}
	if phase != pluginInstallReplacing && phase != pluginInstallCommitted {
		return fmt.Errorf("unknown transaction phase %q for %s", phase, fqid)
	}

	currentValid, currentErr := pm.pluginArchiveMatchesID(target, pluginID)
	backupValid, backupErr := false, error(nil)
	if meta.HadBackup {
		if _, err := os.Stat(backup); err != nil {
			backupErr = err
		} else {
			backupValid, backupErr = pm.pluginArchiveMatchesID(backup, pluginID)
		}
	}

	if phase == pluginInstallCommitted && currentValid {
		return pm.removeRecoveredTransaction(txnDir)
	}
	if phase == pluginInstallCommitted && !meta.HadBackup {
		if _, err := os.Stat(target); err == nil {
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("remove invalid committed archive %s: %w", target, err)
			}
		}
		if err := pm.removeRecoveredTransaction(txnDir); err != nil {
			return err
		}
		return fmt.Errorf("committed plugin archive %s is invalid: %w", fqid, currentErr)
	}

	if meta.HadBackup && backupValid {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove interrupted plugin archive %s: %w", target, err)
		}
		if err := os.Rename(backup, target); err != nil {
			return fmt.Errorf("restore plugin backup for %s: %w", fqid, err)
		}
		if err := syncInstallDirectory(installDir); err != nil {
			return fmt.Errorf("sync recovered plugin archive %s: %w", fqid, err)
		}
		return pm.removeRecoveredTransaction(txnDir)
	}
	if meta.HadBackup && !backupValid {
		if currentValid {
			var pathErr *os.PathError
			if errors.As(backupErr, &pathErr) && !errors.Is(backupErr, os.ErrNotExist) {
				return fmt.Errorf("inspect transaction backup for %s; preserving journal for retry: %w",
					fqid, backupErr)
			}
			removeErr := pm.removeRecoveredTransaction(txnDir)
			if errors.Is(backupErr, os.ErrNotExist) {
				return removeErr
			}
			return errorsJoin(
				fmt.Errorf("transaction backup for %s is invalid: %w", fqid, backupErr),
				removeErr,
			)
		}
		if currentErr == nil && backupErr == nil {
			return fmt.Errorf("no valid installed or backup archive for %s (both archives were invalid without an error)",
				fqid)
		}
		if currentErr == nil {
			return fmt.Errorf("no valid installed or backup archive for %s (current archive was invalid without an error; backup: %w)",
				fqid, backupErr)
		}
		if backupErr == nil {
			return fmt.Errorf("no valid installed or backup archive for %s (current: %w; backup archive was invalid without an error)",
				fqid, currentErr)
		}
		return fmt.Errorf("no valid installed or backup archive for %s (current: %w; backup: %w)",
			fqid, currentErr, backupErr)
	}

	// A pre-commit first install has no prior archive to restore.
	if _, err := os.Stat(target); err == nil {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove uncommitted first-install archive %s: %w", target, err)
		}
	}
	if err := syncInstallDirectory(installDir); err != nil {
		return fmt.Errorf("sync first-install rollback %s: %w", fqid, err)
	}
	return pm.removeRecoveredTransaction(txnDir)
}

func (pm *PluginManager) pluginArchiveMatchesID(path, pluginID string) (bool, error) {
	ctrl, err := pm.readPluginControlFromZip(path)
	if err != nil {
		return false, err
	}
	if ctrl.ID != pluginID {
		return false, fmt.Errorf("manifest ID %q does not match %q", ctrl.ID, pluginID)
	}
	return true, nil
}

func (pm *PluginManager) removeRecoveredTransaction(txnDir string) error {
	if err := pm.removeInstallDirectory(txnDir); err != nil {
		return fmt.Errorf("remove recovered plugin transaction %s: %w", txnDir, err)
	}
	return nil
}

func errorsJoin(first, second error) error {
	return errors.Join(first, second)
}
func isCanonicalPluginArchiveFilename(filename string) bool {
	if !strings.HasSuffix(filename, ".zip") {
		return false
	}
	id, _, hasOrigin := strings.Cut(strings.TrimSuffix(filepath.Base(filename), ".zip"), "@")
	return hasOrigin && id != ""
}
