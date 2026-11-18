package checkout

import (
	"errors"
	"testing"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/service"
)

// gatewayTarget is a signed target whose Gateway answers treasury() with gwTreasury, or fails
// the call when callErr is set.
func gatewayTarget(gwTreasury evm.Address, callErr error) signedTarget {
	fc := evmfake.New(8453)
	fc.CallHandler = func(evmrpc.CallMsg, uint64) ([]byte, error) {
		if callErr != nil {
			return nil, callErr
		}
		w := evm.AddressWord(gwTreasury)
		return w[:], nil
	}
	q := chainwatch.NewQuorum("base", []chainwatch.Provider{fc.Provider("a"), fc.Provider("b")}, chainwatch.Options{})
	return signedTarget{cc: Chain{Chain: chain.Chain{ID: "base"}, Quorum: q}, gateway: evm.Address{0x6a}, tre: evm.Address{0x11}}
}

// unavailableBody checks err is the signed path's usual 503 and returns it.
func unavailableBody(t *testing.T, err error) *service.Error {
	t.Helper()
	var e *service.Error
	if !errors.As(err, &e) || e.Kind != service.KindUnavailable || e.Code != service.CodeServiceUnavailable ||
		e.Message != "signed payments are temporarily unavailable" {
		t.Fatalf("error %#v", err)
	}
	return e
}

// A Gateway whose immutable treasury is not the configured one turns the signed path off with
// the same 503 the payer always sees, but the error keeps why and a critical alert is raised.
func TestGatewayTreasuryMismatchKeepsItsCauseAndAlerts(t *testing.T) {
	rec := &alert.Recorder{}
	c := (&Checkout{gwOK: map[string]evm.Address{}}).WithAlerts(rec)
	err := c.checkGateway(t.Context(), gatewayTarget(evm.Address{0x77}, nil))
	e := unavailableBody(t, err)
	if !errors.Is(err, errTreasuryMismatch) || e.Cause == nil {
		t.Fatalf("cause %v", e.Cause)
	}
	var crit bool
	for _, a := range rec.All() {
		crit = crit || (a.Kind == "gateway_treasury_mismatch" && a.Severity == alert.Critical && a.Chain == "base")
	}
	if !crit {
		t.Fatalf("alerts %v", rec.Kinds())
	}
}

func TestGatewayCallFailureKeepsItsCause(t *testing.T) {
	boom := errors.New("provider down")
	rec := &alert.Recorder{}
	c := (&Checkout{gwOK: map[string]evm.Address{}}).WithAlerts(rec)
	err := c.checkGateway(t.Context(), gatewayTarget(evm.Address{0x11}, boom))
	if e := unavailableBody(t, err); e.Cause == nil || errors.Is(err, errTreasuryMismatch) {
		t.Fatalf("cause %v", e.Cause)
	}
	if len(rec.All()) != 0 {
		t.Fatalf("alerts %v", rec.Kinds())
	}
	// Two failures are two errors: nothing is shared between requests.
	if err2 := c.checkGateway(t.Context(), gatewayTarget(evm.Address{0x11}, boom)); err2 == err {
		t.Fatal("the same error value served twice")
	}
}

func TestMatchingGatewayTreasuryPasses(t *testing.T) {
	c := &Checkout{gwOK: map[string]evm.Address{}}
	if err := c.checkGateway(t.Context(), gatewayTarget(evm.Address{0x11}, nil)); err != nil {
		t.Fatal(err)
	}
}
