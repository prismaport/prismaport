package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
)

// SolanaEnv is the configuration of one Solana cluster, read from PRISMAPORT_CHAIN_<ID>_*.
type SolanaEnv struct {
	ID      string
	RPCURLs []string
	// Program is the prismaport_forwarder program id; with RentReceiver it enables real
	// deposit addresses.
	Program      *solana.PublicKey
	RentReceiver string
	// Genesis is the cluster's genesis hash; every provider must serve it.
	Genesis string
	// Credit is the commitment at which a payment counts: confirmed (default, a supermajority
	// voted on the block, ~1 s) or finalized (~13 s). Either way the payment is re-checked at
	// finalized and reversed if its slot did not survive.
	Credit        solrpc.Commitment
	PriorityFee   uint64
	MinGasBalance uint64
	// Scan is the cluster's scan intervals (PRISMAPORT_CHAIN_<ID>_SCAN_*, else the global
	// PRISMAPORT_SCAN_*).
	Scan scan.Intervals
	// FinalityDelay is how long after a payment was detected or a flush landed its first
	// finalized-slot read is due (PRISMAPORT_CHAIN_<ID>_FINALITY_DELAY, default 20s, D49).
	FinalityDelay time.Duration
}

// defaultSolanaFinalityDelay is the wait before the first finality check on a Solana cluster
// (finality trails confirmation by about 13 s).
const defaultSolanaFinalityDelay = 20 * time.Second

var solanaGenesis = map[string]string{
	"solana":        "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
	"solana-devnet": "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",
}

// HasRPC reports whether at least two Solana RPC endpoints are configured, the minimum for a
// quorum.
func (c SolanaEnv) HasRPC() bool { return len(c.RPCURLs) >= 2 }

// Addresses reports whether real deposit addresses can be derived.
func (c SolanaEnv) Addresses() bool { return c.Program != nil && c.RentReceiver != "" }

func loadSolanaChains(reg *chain.Registry, intervals scan.Intervals) (map[string]SolanaEnv, error) {
	out := map[string]SolanaEnv{}
	for _, live := range []bool{true, false} {
		for _, c := range reg.ForMode(live) {
			if c.Family != chain.Solana {
				continue
			}
			se, set, err := loadSolanaChain(c.ID, intervals)
			if err != nil {
				return nil, fmt.Errorf("chain %s: %w", c.ID, err)
			}
			if set {
				out[c.ID] = se
			}
		}
	}
	return out, nil
}

func loadSolanaChain(id string, intervals scan.Intervals) (SolanaEnv, bool, error) {
	p := envKey(id)
	se := SolanaEnv{ID: id, Genesis: solanaGenesis[id], Credit: solrpc.Confirmed, PriorityFee: 10_000,
		MinGasBalance: 50_000_000, FinalityDelay: defaultSolanaFinalityDelay}
	var set bool
	var err error
	if se.RPCURLs, set, err = rpcURLs(p); err != nil {
		return se, false, err
	}
	if v := os.Getenv(p + "PROGRAM_ID"); v != "" {
		set = true
		k, err := solana.ParsePublicKey(v)
		if err != nil || k.IsZero() {
			return se, false, fmt.Errorf("%sPROGRAM_ID: invalid public key", p)
		}
		se.Program = &k
	}
	if v := os.Getenv(p + "RENT_RECEIVER"); v != "" {
		set = true
		if err := destination.ValidateSolanaFormat(v); err != nil {
			return se, false, fmt.Errorf("%sRENT_RECEIVER: %w", p, err)
		}
		se.RentReceiver = v
	}
	if (se.Program == nil) != (se.RentReceiver == "") {
		return se, false, fmt.Errorf("%sPROGRAM_ID and %sRENT_RECEIVER are set together: both are committed in every deposit address", p, p)
	}
	if v := os.Getenv(p + "GENESIS_HASH"); v != "" {
		se.Genesis = v
	}
	if se.HasRPC() && se.Genesis == "" {
		return se, false, fmt.Errorf("%sGENESIS_HASH is required for this cluster", p)
	}
	if v := os.Getenv(p + "CREDIT"); v != "" {
		switch solrpc.Commitment(v) {
		case solrpc.Confirmed, solrpc.Finalized:
			se.Credit = solrpc.Commitment(v)
		default:
			return se, false, fmt.Errorf("%sCREDIT must be confirmed or finalized", p)
		}
	}
	for _, n := range []struct {
		name string
		dst  *uint64
	}{{"PRIORITY_FEE_MICROLAMPORTS", &se.PriorityFee}, {"MIN_GAS_BALANCE_LAMPORTS", &se.MinGasBalance}} {
		if v := os.Getenv(p + n.name); v != "" {
			x, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return se, false, fmt.Errorf("%s%s must be a non-negative integer", p, n.name)
			}
			*n.dst = x
		}
	}
	if err := positiveDuration(p+"FINALITY_DELAY", &se.FinalityDelay); err != nil {
		return se, false, err
	}
	if se.Scan, err = chainIntervals(p, intervals); err != nil {
		return se, false, err
	}
	return se, set, nil
}

// SolanaGasKey loads the fee payer of flushes and connected-wallet payments.
func (e Env) SolanaGasKey() (*solana.Keypair, error) {
	return solana.LoadKeypair(e.SolanaGasKeyFile, os.Getenv("PRISMAPORT_SOLANA_GAS_KEY"))
}
