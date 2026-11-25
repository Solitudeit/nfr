package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/proto"
)

func TestHandleProbeTCPReachableAndRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// Obtain a closed port for connection refused test.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedAddr := closedLn.Addr().String()
	_ = closedLn.Close()

	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	defer cliConn.Close()

	a := New(Options{})
	a.ctrl = cliConn

	ctx := context.Background()
	a.handleProbe(ctx, &proto.Frame{
		Type:     proto.TypeProbe,
		Code:     "p-ok",
		NodeID:   "t1",
		Message:  ln.Addr().String(),
		Protocol: "tcp",
	})

	_ = srvConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := proto.ReadFrame(srvConn)
	if err != nil {
		t.Fatalf("read ok result: %v", err)
	}
	if f.Type != proto.TypeProbeResult || f.Code != "p-ok" || f.Status != proto.ProbeOK {
		t.Fatalf("unexpected ok frame: %+v", f)
	}

	a.handleProbe(ctx, &proto.Frame{
		Type:     proto.TypeProbe,
		Code:     "p-fail",
		NodeID:   "t2",
		Message:  closedAddr,
		Protocol: "tcp",
	})
	_ = srvConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err = proto.ReadFrame(srvConn)
	if err != nil {
		t.Fatalf("read fail result: %v", err)
	}
	if f.Type != proto.TypeProbeResult || f.Code != "p-fail" || f.Status != proto.ProbeFailed || f.Message == "" {
		t.Fatalf("unexpected fail frame: %+v", f)
	}
}

func TestHandleProbePoolSaturationReturnsBusyWithoutBlocking(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	defer cliConn.Close()

	a := New(Options{})
	a.ctrl = cliConn
	// Pre-fill all 4 worker slots to simulate 4 active slow probes.
	for i := 0; i < maxConcurrentProbes; i++ {
		a.probeSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentProbes; i++ {
			<-a.probeSem
		}
	}()

	start := time.Now()
	a.handleProbe(context.Background(), &proto.Frame{
		Type:     proto.TypeProbe,
		Code:     "p-busy",
		NodeID:   "t5",
		Message:  "127.0.0.1:1",
		Protocol: "tcp",
	})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("handleProbe blocked when worker pool was saturated")
	}

	_ = srvConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := proto.ReadFrame(srvConn)
	if err != nil {
		t.Fatalf("read busy result: %v", err)
	}
	if f.Type != proto.TypeProbeResult || f.Code != "p-busy" || f.Status != proto.ProbeBusy {
		t.Fatalf("unexpected busy frame: %+v", f)
	}
}
