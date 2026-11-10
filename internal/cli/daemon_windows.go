//go:build windows

package cli

import "syscall"

// daemonSysProcAttr returns nil on Windows: the server runs in the foreground
// there (the daemonize path is guarded by runtime.GOOS).
func daemonSysProcAttr() *syscall.SysProcAttr { return nil }
