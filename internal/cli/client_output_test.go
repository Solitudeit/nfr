package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

// The join summary is the only place that tells an operator how to bring the
// client back later: the join code is one-shot, so a reboot recovers through
// the saved config. Capture stdout so the wording is actually asserted.
func TestJoinSummaryDocumentsConfigRestart(t *testing.T) {
	out := captureStdout(t, func() {
		printJoinSummary("mynode", "nodeabc", "tunnel.example.com", "/etc/nfr/agent.toml", "pinned")
	})
	for _, want := range []string{
		"nfrc -c /etc/nfr/agent.toml",
		"agent.toml",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("join summary is missing %q:\n%s", want, out)
		}
	}
}

// The --no-start variant must keep telling the operator the exact command that
// resumes the saved configuration, since that is the systemd path.
func TestJoinSummaryNoStartShowsRestartCommand(t *testing.T) {
	out := captureStdout(t, func() {
		printJoinSummaryNoStart("/etc/nfr/agent.toml")
	})
	if !strings.Contains(out, "nfrc -c /etc/nfr/agent.toml") {
		t.Fatalf("no-start summary does not show the restart command:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	origProgram := Program
	Program = "nfrc"
	defer func() { Program = origProgram }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}
