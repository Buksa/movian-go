//go:build !android && !linux

package core

// registerPlatformBackends — no platform-specific backends off android/linux.
// Linux registers the DVB backend in platform_backends_linux.go.
func (bs *BackendSystem) registerPlatformBackends() {}
