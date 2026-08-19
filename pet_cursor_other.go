//go:build !windows

package main

import "errors"

func windowsLogicalCursorPosition() (int32, int32, error) {
	return 0, 0, errors.New("real-mouse pet look is only available on Windows")
}
