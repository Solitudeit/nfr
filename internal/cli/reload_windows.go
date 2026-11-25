//go:build windows

package cli

import "os"

func notifyReload() (<-chan os.Signal, func()) {
	return nil, func() {}
}
