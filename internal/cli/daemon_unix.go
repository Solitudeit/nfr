//go:build !windows

package cli

import "syscall"

// daemonSysProcAttr detaches the child from the controlling terminal.
// Setsid is Unix-only; callers must stay behind a !windows guard.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
