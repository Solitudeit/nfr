//go:build android

package log

import (
	"os"
	"os/exec"
	"strings"
	"time"
	_ "time/tzdata"
)

// init resolves the device timezone on Android/Termux when CGO is disabled.
// Without CGO, Go cannot read Android's binary tzdata via bionic and falls back
// to UTC unless TZ is explicitly exported. Reading persist.sys.timezone restores
// the device's real local clock for client log timestamps.
func init() {
	if strings.TrimSpace(os.Getenv("TZ")) != "" {
		return
	}
	name, offset := time.Now().Zone()
	if name != "UTC" || offset != 0 {
		return
	}
	out, err := exec.Command("getprop", "persist.sys.timezone").Output()
	if err != nil {
		return
	}
	tz := strings.TrimSpace(string(out))
	if tz == "" {
		return
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		time.Local = loc
	}
}
