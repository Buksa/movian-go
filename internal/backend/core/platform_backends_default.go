//go:build !android

package core

// registerPlatformBackends — no platform-specific backends off android.
func (bs *BackendSystem) registerPlatformBackends() {}
