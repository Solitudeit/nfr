//go:build windows

package server

import "errors"

func execSelf(exePath string, args []string) error {
	return errors.New("in-place exec restart is not supported on Windows; please restart the service")
}
