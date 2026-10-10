//go:build !linux

package dvb

// Non-Linux stub: no /dev/dvb API exists off Linux, so enumeration is
// empty and Adapters/AdapterCount report zero. Android uses the TIF
// backend instead (platform_tv_android.go).

// Adapters returns an empty list — no DVB device support on this OS.
func Adapters() ([]*Adapter, error) { return nil, nil }

// AdapterCount reports 0 — no DVB support on this OS.
func AdapterCount() int { return 0 }
