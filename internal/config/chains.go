package config

import (
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/scan"
)

// ChainEnv is the on-chain configuration of one network, read from
// PRISMAPORT_CHAIN_<ID>_* where <ID> is the chain id upper-cased with '-' as '_'.
type ChainEnv struct {
	ID string
	// RPCURLs are independent providers. Credit needs two of them to agree, so the detector,
	// flusher and relayer only run with at least two.
	RPCURLs []string
	Factory *evm.Address
	Gateway *evm.Address
	// Confirmations is the depth a payment needs to count (default
	// chainwatch.DefaultConfirmations); zero with UseSafeHead means the chain's safe tag decides
	// instead.
	Confirmations uint64
	UseSafeHead   bool
	// MaxFeePerGas caps what the gas wallet pays per gas, in wei. A quote above it is not sent.
	MaxFeePerGas *big.Int
	// MinGasBalance raises an alert when the gas wallet holds less, in wei.
	MinGasBalance *big.Int
	// Scan is the chain's scan intervals (PRISMAPORT_CHAIN_<ID>_SCAN_*, else the global
	// PRISMAPORT_SCAN_*).
	Scan scan.Intervals
	// BlockTime turns the time since a network was revealed into blocks: a deposit address is
	// rescanned from the head at its reveal minus ReanchorMargin blocks
	// (PRISMAPORT_CHAIN_<ID>_BLOCK_TIME, _REANCHOR_MARGIN).
	BlockTime      time.Duration
	ReanchorMargin uint64
	// FinalityDelay is how long after a payment was detected or a flush mined its first
	// finalized-head read is due (PRISMAPORT_CHAIN_<ID>_FINALITY_DELAY, default 15m, D49).
	FinalityDelay time.Duration
	// Multicall3 overrides the registry's Multicall3 contract (PRISMAPORT_CHAIN_<ID>_MULTICALL3);
	// nil keeps the registry's.
	Multicall3 *evm.Address
	// MaxLogRange bounds one eth_getLogs block range for the detector and the sender; zero keeps
	// their default (chainwatch.DefaultMaxLogRange, 500). The config has one RPC list per chain and both quorum
	// providers are asked for identical ranges, so it must fit the stricter provider.
	MaxLogRange uint64
}

func envKey(chainID string) string {
	return "PRISMAPORT_CHAIN_" + strings.ToUpper(strings.ReplaceAll(chainID, "-", "_")) + "_"
}

// Defaults when a variable is unset. A payment counts after a fixed number of blocks rather than
// at the safe head (minutes behind on Base): the pilot accepts the small reorg risk of the
// unsafe head for a checkout that turns paid in seconds, and the finality re-check reverses
// whatever still moves. CONFIRMATIONS=safe restores the slower policy per chain.
var defaultConfirmations = map[string]uint64{
	"base":         chainwatch.DefaultConfirmations,
	"base-sepolia": chainwatch.DefaultConfirmations,
}

// defaultBlockTime is the block interval of each EVM chain (BLOCK_TIME overrides it); 2s for a
// chain not listed.
var defaultBlockTime = map[string]time.Duration{
	"base":         2 * time.Second,
	"base-sepolia": 2 * time.Second,
}

// DefaultReanchorMargin is the number of blocks before the head at a network's reveal that its
// deposit address is rescanned from.
const DefaultReanchorMargin = 30

// defaultFinalityDelay is the wait before the first finality check of an EVM chain not listed in
// defaultFinality (Base and Base Sepolia finalize in about 15-20 minutes).
const defaultFinalityDelay = 15 * time.Minute

var defaultFinality = map[string]time.Duration{
	"base":         15 * time.Minute,
	"base-sepolia": 15 * time.Minute,
}

var defaultMaxFee = map[string]*big.Int{
	"base":         big.NewInt(5_000_000_000), // 5 gwei; Base usually charges well under 0.1 gwei
	"base-sepolia": big.NewInt(5_000_000_000),
}

func loadChains(reg *chain.Registry, intervals scan.Intervals) (map[string]ChainEnv, error) {
	out := map[string]ChainEnv{}
	for _, live := range []bool{true, false} {
		for _, c := range reg.ForMode(live) {
			if c.Family != chain.EVM {
				continue
			}
			ce, set, err := loadChain(c.ID, intervals)
			if err != nil {
				return nil, fmt.Errorf("chain %s: %w", c.ID, err)
			}
			if set {
				out[c.ID] = ce
			}
		}
	}
	return out, nil
}

// rpcURLs reads <p>RPC_URLS, the comma-separated providers of an EVM or Solana chain: at least
// two independent ones. set reports that the variable is present.
func rpcURLs(p string) (urls []string, set bool, err error) {
	v := os.Getenv(p + "RPC_URLS")
	if v == "" {
		return nil, false, nil
	}
	for _, u := range strings.Split(v, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) < 2 {
		return urls, false, fmt.Errorf("%sRPC_URLS needs at least two independent providers", p)
	}
	return urls, true, nil
}

func loadChain(id string, intervals scan.Intervals) (ChainEnv, bool, error) {
	p := envKey(id)
	ce := ChainEnv{ID: id, Confirmations: defaultConfirmations[id], MaxFeePerGas: defaultMaxFee[id],
		MinGasBalance: big.NewInt(1_000_000_000_000_000), // 0.001 ETH
		BlockTime:     defaultBlockTime[id], ReanchorMargin: DefaultReanchorMargin, FinalityDelay: defaultFinality[id]}
	if ce.BlockTime == 0 {
		ce.BlockTime = 2 * time.Second
	}
	if ce.FinalityDelay == 0 {
		ce.FinalityDelay = defaultFinalityDelay
	}
	if ce.Confirmations == 0 {
		ce.Confirmations = chainwatch.DefaultConfirmations
	}
	if ce.MaxFeePerGas == nil {
		ce.MaxFeePerGas = big.NewInt(5_000_000_000)
	}
	var set bool
	var err error
	if ce.RPCURLs, set, err = rpcURLs(p); err != nil {
		return ce, false, err
	}
	for _, a := range []struct {
		name string
		dst  **evm.Address
	}{{"FACTORY", &ce.Factory}, {"GATEWAY", &ce.Gateway}, {"MULTICALL3", &ce.Multicall3}} {
		v := os.Getenv(p + a.name)
		if v == "" {
			continue
		}
		set = true
		addr, err := evm.ParseAddress(v)
		if err != nil || addr.IsZero() {
			return ce, false, fmt.Errorf("%s%s: invalid address", p, a.name)
		}
		*a.dst = &addr
	}
	if v := os.Getenv(p + "CONFIRMATIONS"); v != "" {
		if v == "safe" {
			ce.UseSafeHead, ce.Confirmations = true, 0
		} else {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil || n == 0 {
				return ce, false, fmt.Errorf("%sCONFIRMATIONS must be \"safe\" or a positive integer", p)
			}
			ce.UseSafeHead, ce.Confirmations = false, n
		}
	}
	for _, w := range []struct {
		name string
		dst  **big.Int
	}{{"MAX_FEE_WEI", &ce.MaxFeePerGas}, {"MIN_GAS_BALANCE_WEI", &ce.MinGasBalance}} {
		if v := os.Getenv(p + w.name); v != "" {
			n, ok := new(big.Int).SetString(v, 10)
			if !ok || n.Sign() <= 0 {
				return ce, false, fmt.Errorf("%s%s must be a positive integer", p, w.name)
			}
			*w.dst = n
		}
	}
	if ce.Scan, err = chainIntervals(p, intervals); err != nil {
		return ce, false, err
	}
	if err := positiveDuration(p+"BLOCK_TIME", &ce.BlockTime); err != nil {
		return ce, false, err
	}
	if err := positiveDuration(p+"FINALITY_DELAY", &ce.FinalityDelay); err != nil {
		return ce, false, err
	}
	if v := os.Getenv(p + "REANCHOR_MARGIN"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return ce, false, fmt.Errorf("%sREANCHOR_MARGIN must be a non-negative integer (blocks)", p)
		}
		ce.ReanchorMargin = n
	}
	if v := os.Getenv(p + "MAX_LOG_RANGE"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			return ce, false, fmt.Errorf("%sMAX_LOG_RANGE must be a positive integer", p)
		}
		ce.MaxLogRange = n
	}
	return ce, set, nil
}

// HasRPC reports whether the chain layer (detector, flusher, relayer) can run.
func (c ChainEnv) HasRPC() bool { return len(c.RPCURLs) >= 2 }
