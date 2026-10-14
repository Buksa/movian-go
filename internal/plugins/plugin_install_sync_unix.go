//go:build !windows

package plugins

import "os"

// syncInstallDirectory requests persistence of directory-entry updates. Sync
// errors are returned; power-loss guarantees depend on the platform/filesystem.
func syncInstallDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
