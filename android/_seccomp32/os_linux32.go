// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Movian -overlay replacement for runtime/os_linux32.go (android/arm).
// Tracks go1.26.6+ runtime: upstream now picks time32/time64 once at
// startup via configure64bitsTimeOn32BitsArchitectures — uname >= 5.1
// means time64, else a futex_time64 probe. We force time32
// unconditionally: the app seccomp filter on Android 8 TV boxes can
// answer SIGSYS to the *_time64 syscalls regardless of what uname
// claims, and the time32 variants exist on every kernel we support.

//go:build linux && (386 || arm || mips || mipsle || (gccgo && (ppc || s390)))

package runtime

import (
	"unsafe"
)

// configure64bitsTimeOn32BitsArchitectures decides whether to use the
// 64-bit time variants of futex and timer_settime on 32-bit Linux.
// Movian overlay: always time32 — see the header comment.
func configure64bitsTimeOn32BitsArchitectures() {
	use64bitsTimeOn32bits = false
}

//go:noescape
func futex_time32(addr unsafe.Pointer, op int32, val uint32, ts *timespec32, addr2 unsafe.Pointer, val3 uint32) int32

//go:noescape
func futex_time64(addr unsafe.Pointer, op int32, val uint32, ts *timespec, addr2 unsafe.Pointer, val3 uint32) int32

var use64bitsTimeOn32bits bool

//go:nosplit
func futex(addr unsafe.Pointer, op int32, val uint32, ts *timespec, addr2 unsafe.Pointer, val3 uint32) int32 {
	if use64bitsTimeOn32bits {
		return futex_time64(addr, op, val, ts, addr2, val3)
	}
	// Downgrade ts.
	var ts32 timespec32
	var pts32 *timespec32
	if ts != nil {
		ts32.setNsec(ts.tv_sec*1e9 + int64(ts.tv_nsec))
		pts32 = &ts32
	}
	return futex_time32(addr, op, val, pts32, addr2, val3)
}

//go:noescape
func timer_settime32(timerid int32, flags int32, new, old *itimerspec32) int32

//go:noescape
func timer_settime64(timerid int32, flags int32, new, old *itimerspec) int32

//go:nosplit
func timer_settime(timerid int32, flags int32, new, old *itimerspec) int32 {
	if use64bitsTimeOn32bits {
		return timer_settime64(timerid, flags, new, old)
	}

	var newts, oldts itimerspec32
	var new32, old32 *itimerspec32

	if new != nil {
		newts.it_interval.setNsec(new.it_interval.tv_sec*1e9 + int64(new.it_interval.tv_nsec))
		newts.it_value.setNsec(new.it_value.tv_sec*1e9 + int64(new.it_value.tv_nsec))
		new32 = &newts
	}

	if old != nil {
		oldts.it_interval.setNsec(old.it_interval.tv_sec*1e9 + int64(old.it_interval.tv_nsec))
		oldts.it_value.setNsec(old.it_value.tv_sec*1e9 + int64(old.it_value.tv_nsec))
		old32 = &oldts
	}

	return timer_settime32(timerid, flags, new32, old32)
}
