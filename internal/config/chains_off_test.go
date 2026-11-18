package config

import (
	"slices"
	"testing"
	"time"
)

func TestChainsOff(t *testing.T) {
	t.Setenv("PRISMAPORT_CHAINS_OFF", "")
	off, err := loadChainsOff()
	if err != nil || len(off) != 0 || len(Env{}.Registry().ForMode(true)) != 2 {
		t.Fatalf("none off: %v %v", off, err)
	}
	t.Setenv("PRISMAPORT_CHAINS_OFF", " solana, solana-devnet ,solana")
	off, err = loadChainsOff()
	if err != nil || !slices.Equal(off, []string{"solana", "solana-devnet"}) {
		t.Fatalf("solana off: %v %v", off, err)
	}
	reg := Env{ChainsOff: off}.Registry()
	for _, live := range []bool{true, false} {
		for _, c := range reg.ForMode(live) {
			if c.Family != "evm" {
				t.Fatalf("%s still served", c.ID)
			}
		}
	}
	if _, ok := reg.Get("solana"); ok {
		t.Fatal("solana still in the registry")
	}
	if _, ok := reg.Get("base"); !ok {
		t.Fatal("base went off with solana")
	}
	t.Setenv("PRISMAPORT_CHAINS_OFF", "solana,sol")
	if _, err := loadChainsOff(); err == nil {
		t.Fatal("an unknown id passed")
	}
}

func TestPauseRefresh(t *testing.T) {
	t.Setenv("PRISMAPORT_CHAIN_PAUSE_REFRESH", "")
	if d, err := loadPauseRefresh(); err != nil || d != 30*time.Second {
		t.Fatalf("default: %s %v", d, err)
	}
	t.Setenv("PRISMAPORT_CHAIN_PAUSE_REFRESH", "0")
	if d, err := loadPauseRefresh(); err != nil || d != 0 {
		t.Fatalf("every use: %s %v", d, err)
	}
	for _, bad := range []string{"-1s", "10m", "soon"} {
		t.Setenv("PRISMAPORT_CHAIN_PAUSE_REFRESH", bad)
		if _, err := loadPauseRefresh(); err == nil {
			t.Fatalf("%s: no error", bad)
		}
	}
}
