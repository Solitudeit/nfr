package server

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

func newTestMultiUserServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.PortRange = [2]int{30000, 39999}

	srv, pw, err := New(Options{Config: cfg, Store: st})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	admin, ok := srv.AdminUser()
	if !ok {
		t.Fatal("expected default admin user")
	}
	return srv, admin.ID, pw
}

func dialTestAgent(t *testing.T, addr, authToken, nodeName string, proxies []proto.ProxySpec) (*yamux.Session, net.Conn, *proto.Frame) {
	t.Helper()
	tc, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{proto.ALPN},
	})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	sess, err := yamux.Client(tc, yamuxConfig())
	if err != nil {
		tc.Close()
		t.Fatalf("yamux.Client: %v", err)
	}
	ctrl, err := sess.Open()
	if err != nil {
		sess.Close()
		t.Fatalf("sess.Open: %v", err)
	}
	if err := proto.WriteFrame(ctrl, &proto.Frame{
		Type:    proto.TypeHello,
		Node:    nodeName,
		OS:      "linux",
		Arch:    "amd64",
		Version: "test",
		Proxies: proxies,
	}); err != nil {
		sess.Close()
		t.Fatalf("write hello: %v", err)
	}
	ch, err := proto.ReadFrame(ctrl)
	if err != nil {
		sess.Close()
		t.Fatalf("read challenge: %v", err)
	}
	if ch.Type != proto.TypeChallenge {
		return sess, ctrl, ch
	}
	mac := auth.MAC(auth.Hash(authToken), ch.Nonce)
	if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeAuth, MAC: mac}); err != nil {
		sess.Close()
		t.Fatalf("write auth: %v", err)
	}
	resp, err := proto.ReadFrame(ctrl)
	if err != nil {
		sess.Close()
		t.Fatalf("read auth response: %v", err)
	}
	return sess, ctrl, resp
}

func TestMultiUserControlPlaneRejectsCrossUserHijack(t *testing.T) {
	srv, adminID, _ := newTestMultiUserServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = srv.Run(ctx)
	}()
	defer func() {
		cancel()
		<-runDone
	}()
	deadline := time.Now().Add(5 * time.Second)
	var ctlAddr string
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		if srv.ctlLn != nil {
			ctlAddr = srv.ctlLn.Addr().String()
		}
		srv.mu.Unlock()
		if ctlAddr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ctlAddr == "" {
		t.Fatal("control listener did not start")
	}

	alice, err := srv.CreateUser(api.CreateUserRequest{Username: "alice", Password: "password123"})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := srv.CreateUser(api.CreateUserRequest{Username: "bob", Password: "password123"})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	aliceTok := srv.UserAuthToken(alice.ID)
	bobTok := srv.UserAuthToken(bob.ID)
	if aliceTok == "" || bobTok == "" || aliceTok == bobTok || aliceTok == srv.UserAuthToken(adminID) {
		t.Fatalf("expected distinct non-empty auth tokens for admin/alice/bob")
	}

	aliceTun, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:  "alice-ssh",
		Type:  store.TunnelTCP,
		Local: "127.0.0.1:22",
	})
	if err != nil {
		t.Fatalf("CreateStandaloneTunnelForOwner: %v", err)
	}

	// Review Focus #1: Bob attempts to hijack Alice's per-tunnel node "t-<aliceTun.ID>"
	sess1, _, resp1 := dialTestAgent(t, ctlAddr, bobTok, "t-"+aliceTun.ID, nil)
	defer sess1.Close()
	if resp1.Type != proto.TypeError {
		t.Fatalf("expected cross-user t-<id> hijack to be rejected, got %+v", resp1)
	}

	// Review Focus #1b: Bob attempts to hijack Alice's tunnel by name in Proxies
	sess2, _, resp2 := dialTestAgent(t, ctlAddr, bobTok, "bob-box", []proto.ProxySpec{{Name: "alice-ssh"}})
	defer sess2.Close()
	if resp2.Type != proto.TypeError {
		t.Fatalf("expected cross-user proxy hijack to be rejected, got %+v", resp2)
	}

	// Alice connects cleanly with her own auth_token to her own tunnel
	sess3, ctrl3, resp3 := dialTestAgent(t, ctlAddr, aliceTok, "t-"+aliceTun.ID, nil)
	defer sess3.Close()
	if resp3.Type != proto.TypeWelcome {
		t.Fatalf("expected alice to connect cleanly, got %+v", resp3)
	}
	// Read initial config push
	_, _ = proto.ReadFrame(ctrl3)

	// Disabling Alice kicks her active session and rejects reconnect
	disabled := true
	if _, err := srv.UpdateUserQuotaAndStatus(alice.ID, api.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatalf("disable alice: %v", err)
	}
	_ = ctrl3.SetReadDeadline(time.Now().Add(2 * time.Second))
	kickFrame, err := proto.ReadFrame(ctrl3)
	if err != nil || kickFrame.Type != proto.TypeKick {
		t.Fatalf("expected TypeKick on user disable, got frame=%+v err=%v", kickFrame, err)
	}

	// Reconnect while disabled must be rejected
	sess4, _, resp4 := dialTestAgent(t, ctlAddr, aliceTok, "t-"+aliceTun.ID, nil)
	defer sess4.Close()
	if resp4.Type != proto.TypeError {
		t.Fatalf("expected disabled user reconnect to be rejected, got %+v", resp4)
	}

	// Re-enable Alice, reconnect, then rotate auth_token -> kicks old session
	disabled = false
	if _, err := srv.UpdateUserQuotaAndStatus(alice.ID, api.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatalf("re-enable alice: %v", err)
	}
	sess5, ctrl5, resp5 := dialTestAgent(t, ctlAddr, aliceTok, "t-"+aliceTun.ID, nil)
	defer sess5.Close()
	if resp5.Type != proto.TypeWelcome {
		t.Fatalf("expected re-enabled alice to connect, got %+v", resp5)
	}
	_, _ = proto.ReadFrame(ctrl5)

	newTok, err := srv.RotateUserAuthToken(alice.ID)
	if err != nil || newTok == aliceTok {
		t.Fatalf("RotateUserAuthToken failed: newTok=%q err=%v", newTok, err)
	}
	_ = ctrl5.SetReadDeadline(time.Now().Add(2 * time.Second))
	kickFrame2, err := proto.ReadFrame(ctrl5)
	if err != nil || kickFrame2.Type != proto.TypeKick {
		t.Fatalf("expected TypeKick on token rotation, got frame=%+v err=%v", kickFrame2, err)
	}

	// Regression: revoked shared auth_token must NOT work via per-node NodeID auth either
	tcRevoked, err := tls.Dial("tcp", ctlAddr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{proto.ALPN}})
	if err != nil {
		t.Fatalf("tls.Dial revoked check: %v", err)
	}
	sessRev, err := yamux.Client(tcRevoked, yamuxConfig())
	if err != nil {
		t.Fatalf("yamux.Client revoked check: %v", err)
	}
	defer sessRev.Close()
	ctrlRev, err := sessRev.Open()
	if err != nil {
		t.Fatalf("sessRev.Open: %v", err)
	}
	_ = proto.WriteFrame(ctrlRev, &proto.Frame{Type: proto.TypeHello, NodeID: resp5.NodeID, Node: "t-" + aliceTun.ID})
	revResp, err := proto.ReadFrame(ctrlRev)
	if err != nil || revResp.Type != proto.TypeError {
		t.Fatalf("expected shared-token node per-node reconnect to be rejected, got frame=%+v err=%v", revResp, err)
	}
}

func TestMultiUserQuotasNonBypassable(t *testing.T) {
	srv, _, _ := newTestMultiUserServer(t)

	alice, err := srv.CreateUser(api.CreateUserRequest{
		Username:       "alice",
		Password:       "password123",
		MaxTunnels:     1,
		MaxBandwidthKB: 512,
		MonthlyQuotaMB: 2,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Bandwidth above MaxBandwidthKB must be rejected
	overBW := 1024
	if _, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:        "too-fast",
		Type:        store.TunnelTCP,
		Local:       "127.0.0.1:8080",
		BandwidthKB: &overBW,
	}); err == nil {
		t.Fatal("expected bandwidth above user quota to fail")
	}

	// Create 1st tunnel (bandwidth omitted -> effective limiter still capped at 512 KB/s)
	t1, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:  "web1",
		Type:  store.TunnelTCP,
		Local: "127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("create 1st tunnel: %v", err)
	}
	st := srv.st.Snapshot()
	stT1, _ := st.TunnelByID(t1.ID)
	lim := srv.limiterFor(*stT1)
	if lim == nil || lim.rateKB != 512 {
		t.Fatalf("expected effective limiter rateKB=512 from user quota, got %+v", lim)
	}

	// Creating 2nd tunnel must fail due to MaxTunnels = 1
	if _, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:  "web2",
		Type:  store.TunnelTCP,
		Local: "127.0.0.1:8081",
	}); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("expected MaxTunnels error, got %v", err)
	}

	// Review Focus #2: Burn > 2 MiB traffic through t1, flushQuotas -> auto-pauses t1
	srv.met.AddBytes(t1.ID, 2<<20, 1<<20) // 3 MiB total > 2 MiB quota
	srv.flushQuotas()

	gotT1, ok := srv.TunnelForOwner(alice.ID, t1.ID)
	if !ok {
		t.Fatal("expected t1 to exist")
	}
	if gotT1.Enabled || !gotT1.QuotaPaused || gotT1.QuotaReason != "user" {
		t.Fatalf("expected t1 paused by user quota, got enabled=%v quota_paused=%v reason=%q", gotT1.Enabled, gotT1.QuotaPaused, gotT1.QuotaReason)
	}

	// User cannot bypass by manually re-enabling t1
	if err := srv.SetTunnelEnabledForOwner(alice.ID, t1.ID, true); err == nil {
		t.Fatal("expected over-quota user re-enable to be rejected")
	}

	// User deletes t1 and tries to recreate an enabled tunnel -> UsedBytes MUST persist & block!
	if err := srv.DeleteTunnelForOwner(alice.ID, t1.ID); err != nil {
		t.Fatalf("DeleteTunnelForOwner: %v", err)
	}
	uAfterDel, _ := srv.UserByID(alice.ID)
	if uAfterDel.UsedBytes < 3<<20 {
		t.Fatalf("expected user UsedBytes to persist after tunnel delete, got %d", uAfterDel.UsedBytes)
	}
	if _, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:  "bypass",
		Type:  store.TunnelTCP,
		Local: "127.0.0.1:8082",
	}); err == nil {
		t.Fatal("expected over-quota user recreating enabled tunnel to be blocked")
	}

	// Create disabled tunnel while over quota succeeds, then admin raises quota -> user can enable
	off := false
	t2, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name:    "paused-one",
		Type:    store.TunnelTCP,
		Local:   "127.0.0.1:8082",
		Enabled: &off,
	})
	if err != nil {
		t.Fatalf("create disabled tunnel while over quota: %v", err)
	}
	// Mark t2 as quota-paused by user to verify auto-resume when admin raises quota
	_ = srv.st.Update(func(st *store.State) error {
		if tt, ok := st.TunnelByID(t2.ID); ok {
			tt.QuotaPaused = true
			tt.QuotaReason = "user"
		}
		return nil
	})

	newQuota := int64(10)
	if _, err := srv.UpdateUserQuotaAndStatus(alice.ID, api.UpdateUserRequest{MonthlyQuotaMB: &newQuota}); err != nil {
		t.Fatalf("raise quota: %v", err)
	}
	gotT2, _ := srv.TunnelForOwner(alice.ID, t2.ID)
	if !gotT2.Enabled || gotT2.QuotaPaused {
		t.Fatalf("expected t2 to auto-resume after admin raised quota, got enabled=%v paused=%v", gotT2.Enabled, gotT2.QuotaPaused)
	}

	// Regression: deleting a tunnel BEFORE the 30s flushQuotas ticker still folds
	// unflushed bytes into User.UsedBytes so delete-recreate cannot bypass quota.
	beforeDel, _ := srv.UserByID(alice.ID)
	srv.met.AddBytes(t2.ID, 512<<10, 512<<10) // +1 MiB unflushed
	if err := srv.DeleteTunnelForOwner(alice.ID, t2.ID); err != nil {
		t.Fatalf("DeleteTunnelForOwner t2: %v", err)
	}
	afterDel, _ := srv.UserByID(alice.ID)
	if afterDel.UsedBytes-beforeDel.UsedBytes < 1<<20 {
		t.Fatalf("unflushed traffic lost on delete: before=%d after=%d", beforeDel.UsedBytes, afterDel.UsedBytes)
	}
}

func TestPerUserTotalInOutTrafficIsolationAndPersistence(t *testing.T) {
	srv, adminID, _ := newTestMultiUserServer(t)

	alice, err := srv.CreateUser(api.CreateUserRequest{Username: "alice", Password: "password123"})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := srv.CreateUser(api.CreateUserRequest{Username: "bob", Password: "password123"})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	tAdmin, err := srv.CreateStandaloneTunnelForOwner(adminID, api.TunnelRequest{
		Name: "admin-svc", Type: store.TunnelTCP, Local: "127.0.0.1:22",
	})
	if err != nil {
		t.Fatalf("create admin tunnel: %v", err)
	}
	tAlice, err := srv.CreateStandaloneTunnelForOwner(alice.ID, api.TunnelRequest{
		Name: "alice-svc", Type: store.TunnelTCP, Local: "127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("create alice tunnel: %v", err)
	}

	// Simulate distinct in/out bytes on admin and alice tunnels (bob has none)
	srv.met.AddBytes(tAdmin.ID, 1000, 2000)
	srv.met.AddBytes(tAlice.ID, 5000, 9000)

	// 1. Live profile reflects unflushed traffic immediately with strict isolation
	pAdmin, _ := srv.UserProfile(adminID)
	pAlice, _ := srv.UserProfile(alice.ID)
	pBob, _ := srv.UserProfile(bob.ID)
	if pAdmin.TotalInBytes != 1000 || pAdmin.TotalOutBytes != 2000 {
		t.Fatalf("admin live totals = (%d, %d), want (1000, 2000)", pAdmin.TotalInBytes, pAdmin.TotalOutBytes)
	}
	if pAlice.TotalInBytes != 5000 || pAlice.TotalOutBytes != 9000 {
		t.Fatalf("alice live totals = (%d, %d), want (5000, 9000)", pAlice.TotalInBytes, pAlice.TotalOutBytes)
	}
	if pBob.TotalInBytes != 0 || pBob.TotalOutBytes != 0 {
		t.Fatalf("bob leaked traffic: (%d, %d)", pBob.TotalInBytes, pBob.TotalOutBytes)
	}

	// 2. After flushQuotas, totals stay identical (zero double counting) and persist in store
	srv.flushQuotas()
	pAliceFlushed, _ := srv.UserProfile(alice.ID)
	if pAliceFlushed.TotalInBytes != 5000 || pAliceFlushed.TotalOutBytes != 9000 {
		t.Fatalf("alice flushed totals = (%d, %d), want (5000, 9000)", pAliceFlushed.TotalInBytes, pAliceFlushed.TotalOutBytes)
	}

	// 3. Add more traffic and delete alice's tunnel -> cumulative totals persist cleanly!
	srv.met.AddBytes(tAlice.ID, 1234, 5678)
	if err := srv.DeleteTunnelForOwner(alice.ID, tAlice.ID); err != nil {
		t.Fatalf("delete alice tunnel: %v", err)
	}
	pAliceAfterDel, _ := srv.UserProfile(alice.ID)
	if pAliceAfterDel.TotalInBytes != 6234 || pAliceAfterDel.TotalOutBytes != 14678 {
		t.Fatalf("alice after delete totals = (%d, %d), want (6234, 14678)", pAliceAfterDel.TotalInBytes, pAliceAfterDel.TotalOutBytes)
	}
}
