//go:build windows

package main

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsCursorPoint struct {
	X int32
	Y int32
}

var (
	petUser32DLL                        = windows.NewLazySystemDLL("user32.dll")
	petGetCursorPosProc                 = petUser32DLL.NewProc("GetCursorPos")
	petSetThreadDPIAwarenessContextProc = petUser32DLL.NewProc("SetThreadDpiAwarenessContext")
)

func windowsLogicalCursorPosition() (int32, int32, error) {
	// DPI_AWARENESS_CONTEXT_UNAWARE_GDISCALED (-5) makes GetCursorPos use the
	// same logical coordinate space as the avatar overlay renderer.
	unawareGDIScaled := ^uintptr(4)
	previous, _, _ := petSetThreadDPIAwarenessContextProc.Call(unawareGDIScaled)
	if previous == 0 {
		return 0, 0, errors.New("SetThreadDpiAwarenessContext failed")
	}
	defer petSetThreadDPIAwarenessContextProc.Call(previous)
	var point windowsCursorPoint
	ok, _, callErr := petGetCursorPosProc.Call(uintptr(unsafe.Pointer(&point)))
	if ok == 0 {
		return 0, 0, callErr
	}
	return point.X, point.Y, nil
}
