//go:build !windows

package server

import (
	"os"
	"syscall"
)

func execSelf(exePath string, args []string) error {
	return syscall.Exec(exePath, args, os.Environ())
}
