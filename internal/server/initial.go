package server

import (
	"os"
	"path/filepath"
)

// initialPasswordFile holds the first-run banner, password included, for the
// machines where the banner is the only copy: under systemd it goes to the
// journal, and a host with journald disabled or a destroyed journal has
// nowhere to read it back. The file is deleted as soon as the operator signs
// in, so the window in which the plaintext password sits on disk is the same
// window in which it is still unused.
const initialPasswordFile = "initial-password.txt"

// InitialPasswordPath is where a control node parks its first-run banner.
func InitialPasswordPath(dataDir string) string {
	return filepath.Join(dataDir, initialPasswordFile)
}

// ClearInitialPassword drops the parked banner after a successful sign-in. It
// is called on every login; a missing file is not an error, because that is
// the normal state after the first one.
func (s *Server) ClearInitialPassword() {
	_ = os.Remove(InitialPasswordPath(s.cfg.DataDir))
}

// DataDir is where this node keeps its state and certificates.
func (s *Server) DataDir() string { return s.cfg.DataDir }

// HasInitialPassword reports whether a parked banner is still waiting to be
// read, so `nfrs status` can point at it.
func HasInitialPassword(dataDir string) bool {
	_, err := os.Stat(InitialPasswordPath(dataDir))
	return err == nil
}
