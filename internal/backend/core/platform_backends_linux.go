//go:build linux && !android

package core

// registerPlatformBackends — platform-specific backends for linux
// (desktop + MGOS). DVB live TV registers only when adapters exist.
func (bs *BackendSystem) registerPlatformBackends() {
	bs.registerDVBLinux()
}
