package config

import (
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/solrpc"
)

const (
	program = "3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya"
	renter  = "CDYn2if6T4voQoxm5mZKRAg5a65ZVzhQ5RK8H8e2DmS8"
)

func TestLoadSolanaChains(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Solana) != 0 {
		t.Fatalf("unset clusters configured: %+v", e.Solana)
	}
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID", program)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("program without rent receiver: %v", err)
	}
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER", renter)
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RPC_URLS", "https://a.example/key, https://b.example/key")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_CREDIT", "confirmed")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_PRIORITY_FEE_MICROLAMPORTS", "5000")
	e, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	s := e.Solana["solana"]
	if !s.Addresses() || !s.HasRPC() || s.Program.String() != program || s.RentReceiver != renter || s.Credit != solrpc.Confirmed ||
		s.PriorityFee != 5000 || s.Genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		t.Fatalf("solana env %+v", s)
	}
	if s.FinalityDelay != 20*time.Second {
		t.Fatalf("default finality delay %v", s.FinalityDelay)
	}
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_FINALITY_DELAY", "45s")
	if e2, err := Load(); err != nil || e2.Solana["solana"].FinalityDelay != 45*time.Second {
		t.Fatalf("finality delay %+v %v", e2.Solana["solana"], err)
	}
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_FINALITY_DELAY", "")
	for name, bad := range map[string]string{"RPC_URLS": "https://one", "CREDIT": "processed", "RENT_RECEIVER": "0x00",
		"PROGRAM_ID": "11111111111111111111111111111111", "SCAN_HOT": "0s",
		"FINALITY_DELAY": "0s"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PRISMAPORT_CHAIN_SOLANA_"+name, bad)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s accepted", name, bad)
			}
		})
	}
}
