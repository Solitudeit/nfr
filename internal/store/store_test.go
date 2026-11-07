package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/auth"
)

func TestOpenCreatesState(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := filepath.Join(dir, "state.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	snap := st.Snapshot()
	if len(snap.ID) != 8 {
		t.Fatalf("instance id %q is not 8 characters", snap.ID)
	}
	if snap.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d", snap.SchemaVersion)
	}
}

func TestUpdatePersistsAndBumpsRevision(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := st.Snapshot().Revision
	now := time.Now().UTC()
	err = st.Update(func(s *State) error {
		s.Nodes = append(s.Nodes, Node{ID: "node1", Name: "edge", CreatedAt: now})
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := st.Snapshot().Revision; got != base+1 {
		t.Fatalf("revision = %d, want %d", got, base+1)
	}

	// A failing update must not persist anything.
	if err := st.Update(func(s *State) error {
		s.Nodes = append(s.Nodes, Node{ID: "node2"})
		return os.ErrPermission
	}); err == nil {
		t.Fatal("expected the update to fail")
	}
	if got := len(st.Snapshot().Nodes); got != 1 {
		t.Fatalf("failed update leaked %d nodes", got)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(again.Snapshot().Nodes) != 1 || again.Snapshot().Revision != base+1 {
		t.Fatalf("state did not survive reopen: %+v", again.Snapshot())
	}
}

func TestQueries(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	now := time.Now().UTC()
	_ = st.Update(func(s *State) error {
		s.Nodes = append(s.Nodes, Node{ID: "n1", Name: "edge", TokenHash: auth.Hash("tok")})
		s.Tunnels = append(s.Tunnels,
			Tunnel{ID: "t1", Name: "web", NodeID: "n1", Type: TunnelTCP, RemotePort: 20001, Enabled: true},
			Tunnel{ID: "t2", Name: "api", NodeID: "n1", Type: TunnelTCP, RemotePort: 20002, Enabled: true})
		s.Leases = append(s.Leases, Lease{ID: "l1", Port: 20002, TunnelID: "t2", ExpiresAt: now.Add(time.Minute)},
			Lease{ID: "l2", Port: 20003, TunnelID: "t2", ExpiresAt: now.Add(-time.Minute)})
		return nil
	})
	_ = st.View(func(s *State) error {
		if tl, ok := s.TunnelByID("t2"); !ok || tl.Name != "api" {
			t.Errorf("TunnelByID failed")
		}
		if n, ok := s.NodeByID("n1"); !ok || n.Name != "edge" {
			t.Errorf("NodeByID failed")
		}
		if n, ok := s.NodeByTokenHash(auth.Hash("tok")); !ok || n.ID != "n1" {
			t.Errorf("NodeByTokenHash failed")
		}
		if tl, ok := s.TunnelOnPort(20001, ""); !ok || tl.ID != "t1" {
			t.Errorf("TunnelOnPort failed")
		}
		if tl, ok := s.TunnelOnPort(20001, "t1"); ok {
			t.Errorf("TunnelOnPort ignored the exclusion, got %q", tl.ID)
		}
		if got := len(s.TunnelsForNode("n1")); got != 2 {
			t.Errorf("TunnelsForNode = %d, want 2", got)
		}
		s.PruneLeases(now)
		if len(s.Leases) != 1 || s.Leases[0].ID != "l1" {
			t.Errorf("PruneLeases kept %+v", s.Leases)
		}
		return nil
	})
}

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := NewID()
		if len(id) != 8 {
			t.Fatalf("id %q is not 8 characters", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
