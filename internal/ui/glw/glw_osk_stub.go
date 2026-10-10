//go:build !android

package glw

import (
	propcore "github.com/czz/movian-go/internal/prop"
	settingscore "github.com/czz/movian-go/internal/settings"
)

// glwPlatformOskSetting — no-op outside Android: the system-IME OSK
// toggle exists only where a platform IME can back it (the real
// implementation lives in glw_android.go).
func glwPlatformOskSetting(sm *settingscore.SettingsManager, s *propcore.Prop) {
}
