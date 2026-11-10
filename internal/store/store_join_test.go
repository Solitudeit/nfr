package store

import (
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/auth"
)

func TestJoinCodeByTunnel(t *testing.T) {
	st := &State{Joins: []auth.JoinCode{{Code: "ABC234", TunnelID: "t1", ExpiresAt: time.Now().Add(time.Minute)}}}
	if _, ok := FindJoinCodeByTunnel(st, "t1"); !ok {
		t.Fatal("want bound code found")
	}
	if _, ok := FindJoinCodeByTunnel(st, "t2"); ok {
		t.Fatal("want none for other tunnel")
	}
	DeleteJoinsByTunnel(st, "t1")
	if _, ok := FindJoinCodeByTunnel(st, "t1"); ok {
		t.Fatal("want deleted")
	}
}
