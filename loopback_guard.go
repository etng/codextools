package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type fileLockGuard interface {
	release() error
	path() string
}

type loopbackPortGuard struct {
	lockFile         fileLockGuard
	listener         net.Listener
	fallbackLockPath string
}

func (guard *loopbackPortGuard) release() {
	if guard == nil {
		return
	}
	if guard.listener != nil {
		_ = guard.listener.Close()
		guard.listener = nil
	}
	if guard.lockFile != nil {
		_ = guard.lockFile.release()
		guard.lockFile = nil
	}
}

func (guard *loopbackPortGuard) fallbackPath() string {
	if guard == nil {
		return ""
	}
	return guard.fallbackLockPath
}

type lockBusyError struct {
	path string
}

func (err *lockBusyError) Error() string {
	return "loopback port guard lock is already held: " + err.path
}

func acquireResilientLoopbackPortGuard(port uint16) (*loopbackPortGuard, error) {
	return acquireResilientLoopbackPortGuardAt(port, stateDir())
}

func acquireResilientLoopbackPortGuardAt(port uint16, root string) (*loopbackPortGuard, error) {
	return acquireResilientLoopbackPortGuardWith(port, root, bindLoopbackPortGuard, tcpPortAccepting)
}

func acquireResilientLoopbackPortGuardWith(
	port uint16,
	root string,
	bind func(uint16) (net.Listener, error),
	canConnect func(uint16) bool,
) (*loopbackPortGuard, error) {
	if port == 0 {
		listener, err := bind(port)
		if err != nil {
			return nil, err
		}
		return &loopbackPortGuard{listener: listener}, nil
	}
	lockFile, err := acquireLoopbackPortLock(port, root)
	if err != nil {
		return nil, err
	}
	listener, err := bind(port)
	if err == nil {
		return &loopbackPortGuard{lockFile: lockFile, listener: listener}, nil
	}
	if isAddrInUseError(err) {
		if canConnect(port) {
			_ = lockFile.release()
			return nil, err
		}
		return &loopbackPortGuard{lockFile: lockFile, fallbackLockPath: lockFile.path()}, nil
	}
	if isPortBindForbiddenError(err) {
		return &loopbackPortGuard{lockFile: lockFile, fallbackLockPath: lockFile.path()}, nil
	}
	_ = lockFile.release()
	return nil, err
}

func acquireLoopbackPortLock(port uint16, root string) (fileLockGuard, error) {
	lockDir := filepath.Join(root, "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(lockDir, fmt.Sprintf("loopback-port-%d.lock", port))
	return tryAcquireFileLock(lockPath)
}

func bindLoopbackPortGuard(port uint16) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func isLoopbackGuardBusyError(err error) bool {
	var busy *lockBusyError
	return errors.As(err, &busy) || isAddrInUseError(err)
}

func isAddrInUseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "address already in use") ||
		strings.Contains(message, "only one usage of each socket address") ||
		strings.Contains(message, "addrinuse")
}

func isPortBindForbiddenError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EACCES) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.Errno(10013) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "permission denied") ||
		strings.Contains(message, "forbidden by its access permissions")
}
