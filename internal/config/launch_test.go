package config

import (
	"strings"
	"testing"
)

func TestLaunchTokenRoundTripAndValidation(t *testing.T) {
	pin := "0fc138e900dc5d9125b3be952e0c1c73551272ba7f664b1d96f582770ec30bbb"
	spec := LaunchSpec{
		ServerAddr: "203.0.113.10:8443",
		AuthToken:  "nfr_t_exampletoken1234567890",
		TunnelID:   "90gv0gv9",
		Pin:        "0F:C1:38:E9:00:DC:5D:91:25:B3:BE:95:2E:0C:1C:73:55:12:72:BA:7F:66:4B:1D:96:F5:82:77:0E:C3:0B:BB",
	}

	tok, err := EncodeLaunchToken(spec)
	if err != nil {
		t.Fatalf("EncodeLaunchToken: %v", err)
	}
	if !strings.HasPrefix(tok, LaunchTokenPrefix) {
		t.Fatalf("token %q missing prefix %q", tok, LaunchTokenPrefix)
	}
	if strings.ContainsAny(tok, " \t\n\"'$&|;") {
		t.Fatalf("token %q contains shell-unsafe characters", tok)
	}

	got, err := DecodeLaunchToken(tok)
	if err != nil {
		t.Fatalf("DecodeLaunchToken: %v", err)
	}
	if got.ServerAddr != spec.ServerAddr || got.AuthToken != spec.AuthToken || got.TunnelID != spec.TunnelID || got.Pin != pin || got.CA {
		t.Fatalf("decoded mismatch: got %+v", got)
	}

	ag := got.AgentConfig()
	if err := ag.Validate(); err != nil {
		t.Fatalf("AgentConfig().Validate(): %v", err)
	}
	if ag.Name != "t-90gv0gv9" {
		t.Fatalf("AgentConfig().Name = %q, want t-90gv0gv9", ag.Name)
	}

	// Public CA mode omits 32-byte pin and produces an even shorter token.
	caTok, err := EncodeLaunchToken(LaunchSpec{
		ServerAddr: "nfr.example.com:8443",
		AuthToken:  "nfr_t_catoken",
		TunnelID:   "abcd1234",
		CA:         true,
	})
	if err != nil {
		t.Fatalf("EncodeLaunchToken(CA): %v", err)
	}
	caGot, err := DecodeLaunchToken(caTok)
	if err != nil {
		t.Fatalf("DecodeLaunchToken(CA): %v", err)
	}
	if !caGot.CA || caGot.Pin != "" || caGot.TunnelID != "abcd1234" {
		t.Fatalf("CA decoded mismatch: %+v", caGot)
	}
	if err := caGot.AgentConfig().Validate(); err != nil {
		t.Fatalf("CA AgentConfig().Validate(): %v", err)
	}

	// Error cases.
	if _, err := EncodeLaunchToken(LaunchSpec{ServerAddr: "1.2.3.4:8443", AuthToken: "k", TunnelID: "t"}); err == nil {
		t.Fatal("expected error when neither pin nor ca is set")
	}
	for _, bad := range []string{"", "badprefix_abc", "nfr1_!!!", "nfr1_AQAAAA"} {
		if _, err := DecodeLaunchToken(bad); err == nil {
			t.Fatalf("expected DecodeLaunchToken(%q) to fail", bad)
		}
	}
}
