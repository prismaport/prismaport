package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
)

// loadChainsOff reads PRISMAPORT_CHAINS_OFF ("solana,solana-devnet"); an id that is not a pilot
// chain is an error, so a typo never leaves a chain on.
func loadChainsOff() ([]string, error) {
	var out []string
	for _, id := range strings.Split(os.Getenv("PRISMAPORT_CHAINS_OFF"), ",") {
		if id = strings.TrimSpace(id); id == "" || slices.Contains(out, id) {
			continue
		}
		if _, ok := chain.Pilot().Get(id); !ok {
			return nil, fmt.Errorf("PRISMAPORT_CHAINS_OFF names %q, which is not a chain: use ids such as base, base-sepolia, solana, solana-devnet", id)
		}
		out = append(out, id)
	}
	return out, nil
}

// Registry is the pilot registry minus the chains this environment switched off.
func (e Env) Registry() *chain.Registry {
	return chain.Pilot().Without(e.ChainsOff...)
}

// maxPauseRefresh bounds PRISMAPORT_CHAIN_PAUSE_REFRESH: a pause must reach every process soon.
const maxPauseRefresh = 5 * time.Minute

// loadPauseRefresh reads PRISMAPORT_CHAIN_PAUSE_REFRESH, how long the api and the worker serve the
// chain pauses (D64) before reading them again: default 30s, 0 reads them on every use.
func loadPauseRefresh() (time.Duration, error) {
	d, err := durationOr("PRISMAPORT_CHAIN_PAUSE_REFRESH", chainpause.DefaultRefresh)
	if err != nil {
		return 0, err
	}
	if d < 0 || d > maxPauseRefresh {
		return 0, fmt.Errorf("PRISMAPORT_CHAIN_PAUSE_REFRESH must be between 0 (read on every use) and %s, got %s", maxPauseRefresh, d)
	}
	return d, nil
}
