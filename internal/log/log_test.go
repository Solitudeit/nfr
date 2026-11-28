package log

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestNewClientWriterFormat(t *testing.T) {
	var buf bytes.Buffer
	lg := NewClientWriter("info", &buf)
	lg.Info("启动 NFR 客户端\n配置: nfrc-ces.toml")
	lg.Warn("连接暂时中断")

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %q", len(lines), buf.String())
	}
	reInfo := regexp.MustCompile(`^\d{2}:\d{2}:\d{2} \[I\] 启动 NFR 客户端$`)
	if !reInfo.MatchString(lines[0]) {
		t.Fatalf("line 0 mismatch: %q", lines[0])
	}
	if lines[1] != "  配置: nfrc-ces.toml" {
		t.Fatalf("line 1 indent mismatch: %q", lines[1])
	}
	reWarn := regexp.MustCompile(`^\d{2}:\d{2}:\d{2} \[W\] 连接暂时中断$`)
	if !reWarn.MatchString(lines[2]) {
		t.Fatalf("line 2 mismatch: %q", lines[2])
	}

	var cbuf bytes.Buffer
	clg := NewClientColorWriter("info", &cbuf, true)
	clg.Info("登录节点成功, RunId: abc12345")
	out := cbuf.String()
	if !strings.HasPrefix(out, ansiDim) || !strings.Contains(out, ansiBlue+"[I]"+ansiReset) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected layered ANSI info line, got %q", out)
	}
}
