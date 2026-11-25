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
	lg.Info("开始启动 NFR 客户端, 当前正在使用配置文件 [nfrc-ces.toml]")
	lg.Warn("连接暂时中断")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %q", len(lines), buf.String())
	}
	reInfo := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} \[I\] 开始启动 NFR 客户端, 当前正在使用配置文件 \[nfrc-ces\.toml\]$`)
	if !reInfo.MatchString(lines[0]) {
		t.Fatalf("line 0 mismatch: %q", lines[0])
	}
	reWarn := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} \[W\] 连接暂时中断$`)
	if !reWarn.MatchString(lines[1]) {
		t.Fatalf("line 1 mismatch: %q", lines[1])
	}
}
