//go:build darwin

package sd

/*
#include <stdint.h>
#include <CoreServices/CoreServices.h>
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// Callback exports invoked from the bonjour trampolines in bonjour.go.
// Handles cross the boundary as uintptr_t cgo.Handle values.

//export goBonjourResolve
func goBonjourResolve(theService C.uintptr_t, info C.uintptr_t) {
	bonjourResolveCallback(
		C.CFNetServiceRef(unsafe.Pointer(uintptr(theService))),
		cgo.Handle(info).Value().(*serviceInstance))
}

//export goBonjourBrowser
func goBonjourBrowser(flags C.CFOptionFlags, domainOrService C.uintptr_t,
	info C.uintptr_t) {
	bonjourBrowserCallback(flags,
		C.CFNetServiceRef(unsafe.Pointer(uintptr(domainOrService))),
		cgo.Handle(info).Value().(*bonjourServiceAux))
}
