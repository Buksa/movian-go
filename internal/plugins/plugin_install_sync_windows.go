//go:build windows

package plugins

import (
	"errors"

	"golang.org/x/sys/windows"
)

// syncInstallDirectory flushes directory-entry changes when Windows supports
// directory handles. Transaction files are synced separately, so successful
// directory flushes still cannot promise power-loss durability on every
// filesystem; unsupported directory flushes are best-effort.
func syncInstallDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.FlushFileBuffers(handle); err != nil {
		// Some Windows filesystems do not support flushing a directory handle.
		// Transaction files themselves are flushed before state transitions.
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return nil
		}
		return err
	}
	return nil
}
