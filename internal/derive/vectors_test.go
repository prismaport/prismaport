package derive

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
)

type vectorFile struct {
	Count         int `json:"count"`
	RejectedCount int `json:"rejectedCount"`
	Rejected      []struct {
		Name           string   `json:"name"`
		Treasury       string   `json:"treasury"`
		FeeBps         uint16   `json:"feeBps"`
		FixedFeeMicros uint64   `json:"fixedFeeMicros"`
		Recipients     []string `json:"recipients"`
		Bps            []uint16 `json:"bps"`
		Tokens         []string `json:"tokens"`
		Error          string   `json:"error"`
		ErrorData      string   `json:"errorData"`
	} `json:"rejected"`
	Vectors []struct {
		Name                 string   `json:"name"`
		Factory              string   `json:"factory"`
		FactoryDeployer      string   `json:"factoryDeployer"`
		FactoryDeployerNonce uint64   `json:"factoryDeployerNonce"`
		Salt                 string   `json:"salt"`
		Treasury             string   `json:"treasury"`
		FeeBps               uint16   `json:"feeBps"`
		FixedFeeMicros       uint64   `json:"fixedFeeMicros"`
		Recipients           []string `json:"recipients"`
		Bps                  []uint16 `json:"bps"`
		Tokens               []string `json:"tokens"`
		Args                 string   `json:"args"`
		ArgsLength           int      `json:"argsLength"`
		Implementation       string   `json:"implementation"`
		InitCode             string   `json:"initCode"`
		InitCodeLength       int      `json:"initCodeLength"`
		InitCodeHash         string   `json:"initCodeHash"`
		Forwarder            string   `json:"forwarder"`
	} `json:"vectors"`
}

// TestCreate2Vectors checks every case of the shared vector file the contracts generate, so the
// Go derivation and the Factory can never drift apart silently.
func TestCreate2Vectors(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/test/vectors/create2.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if vf.Count == 0 || len(vf.Vectors) != vf.Count {
		t.Fatalf("vector count %d, listed %d", vf.Count, len(vf.Vectors))
	}
	base, _ := chain.Pilot().Get("base")
	counts, tokenCounts := map[int]int{}, map[int]int{}
	for _, v := range vf.Vectors {
		counts[len(v.Recipients)]++
		tokenCounts[len(v.Tokens)]++
		t.Run(v.Name, func(t *testing.T) {
			factory, err := evm.ParseAddress(v.Factory)
			if err != nil {
				t.Fatal(err)
			}
			deployer, _ := evm.ParseAddressLoose(v.FactoryDeployer)
			if got := evm.CreateAddress(deployer, v.FactoryDeployerNonce); got != factory {
				t.Fatalf("factory CREATE address %s, want %s", got.Hex(), v.Factory)
			}
			impl := CloneImplementation(factory)
			if impl.Hex() != v.Implementation {
				t.Fatalf("implementation %s, want %s", impl.Hex(), v.Implementation)
			}
			if len(v.Recipients) != len(v.Bps) {
				t.Fatalf("%d recipients, %d shares", len(v.Recipients), len(v.Bps))
			}
			p := Params{Treasury: v.Treasury, Fee: fee.Params{FeeBps: v.FeeBps, FixedFeeMicros: v.FixedFeeMicros}, Tokens: v.Tokens}
			for i := range v.Recipients {
				p.Recipients = append(p.Recipients, payout.Recipient{Address: v.Recipients[i], Bps: v.Bps[i]})
			}
			args, err := PackEVMArgs(p)
			if err != nil {
				t.Fatal(err)
			}
			if "0x"+hex.EncodeToString(args) != v.Args || len(args) != v.ArgsLength || len(args) != 32+22*len(p.Recipients)+20*len(p.Tokens) {
				t.Fatalf("args %x (%d bytes), want %s", args, len(args), v.Args)
			}
			code := CloneInitCode(impl, args)
			if "0x"+hex.EncodeToString(code) != v.InitCode {
				t.Fatalf("init code %x\nwant %s", code, v.InitCode)
			}
			if len(code) != v.InitCodeLength || len(code) != 55+len(args) {
				t.Fatalf("init code is %d bytes, vector %d", len(code), v.InitCodeLength)
			}
			if "0x"+hex.EncodeToString(evm.Keccak256(code)) != v.InitCodeHash {
				t.Fatalf("init code hash mismatch")
			}
			salt, err := evm.ParseWord(v.Salt)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Forwarders(factory).Address(base, salt, p)
			if err != nil {
				t.Fatal(err)
			}
			if got != v.Forwarder {
				t.Fatalf("forwarder %s, want %s", got, v.Forwarder)
			}
		})
	}
	if counts[1] == 0 || counts[2] == 0 || counts[5] == 0 {
		t.Fatalf("the vectors must cover 1, 2 and 5 recipients: %v", counts)
	}
	if tokenCounts[1] == 0 || tokenCounts[2] == 0 || tokenCounts[MaxTokens] == 0 {
		t.Fatalf("the vectors must cover 1, 2 and %d tokens: %v", MaxTokens, tokenCounts)
	}
}

// TestCreate2Rejected: every list the Factory refuses, Go refuses too, with the same error and
// the same argument (the revert data must match byte for byte).
func TestCreate2Rejected(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/test/vectors/create2.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if vf.RejectedCount == 0 || len(vf.Rejected) != vf.RejectedCount {
		t.Fatalf("rejected count %d, listed %d", vf.RejectedCount, len(vf.Rejected))
	}
	base, _ := chain.Pilot().Get("base")
	factory, _ := evm.ParseAddress("0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	for _, v := range vf.Rejected {
		t.Run(v.Name, func(t *testing.T) {
			p := Params{Treasury: v.Treasury, Fee: fee.Params{FeeBps: v.FeeBps, FixedFeeMicros: v.FixedFeeMicros}, Tokens: v.Tokens}
			for i := range v.Recipients {
				p.Recipients = append(p.Recipients, payout.Recipient{Address: v.Recipients[i], Bps: v.Bps[i]})
			}
			_, err := PackEVMArgs(p)
			var tle *TokenListError
			if !errors.As(err, &tle) {
				t.Fatalf("got %v, want %s", err, v.Error)
			}
			if got := "0x" + hex.EncodeToString(tle.RevertData()); got != v.ErrorData {
				t.Fatalf("revert data %s, want %s (%s)", got, v.ErrorData, v.Error)
			}
			if !strings.Contains(err.Error(), v.Error) {
				t.Fatalf("error %q does not name %s", err, v.Error)
			}
			if _, err := Forwarders(factory).Address(base, [32]byte{}, p); !errors.As(err, &tle) {
				t.Fatalf("deriver: got %v, want %s", err, v.Error)
			}
			// Sorting cannot rescue a list the Factory refuses for its count, a zero or a duplicate;
			// only an out-of-order list of distinct tokens becomes valid once canonical.
			sorted, err := SortTokens(chain.EVM, v.Tokens)
			if err != nil {
				t.Fatal(err)
			}
			if CheckTokens(chain.EVM, sorted) == nil && v.Name != "unsorted-tokens" {
				t.Fatalf("%s became valid once sorted", v.Name)
			}
		})
	}
}

func TestPerChain(t *testing.T) {
	r := chain.Pilot()
	base, _ := r.Get("base")
	sepolia, _ := r.Get("base-sepolia")
	factory, _ := evm.ParseAddress("0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	d := PerChain{Chains: map[string]AddressDeriver{"base": Forwarders(factory)}, Fallback: Stub{}}
	var salt [32]byte
	live, err := d.Address(base, salt, params)
	if err != nil {
		t.Fatalf("live base with a factory must derive: %v", err)
	}
	want, _ := Forwarders(factory).Address(base, salt, params)
	if live != want {
		t.Fatal("per-chain deriver not used")
	}
	if _, err := d.Address(sepolia, salt, params); err != nil {
		t.Fatalf("test chain falls back to the stub: %v", err)
	}
	solana, _ := r.Get("solana")
	if _, err := d.Address(solana, salt, params); !errors.Is(err, ErrUnavailable) {
		t.Fatal("solana live must stay unavailable")
	}
	if _, err := (PerChain{}).Address(sepolia, salt, params); !errors.Is(err, ErrUnavailable) {
		t.Fatal("no fallback must be unavailable")
	}
}
