//go:build integration

package solsender_test

import (
	"testing"

	"github.com/prismaport/prismaport/internal/flushfee"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
)

// ADR 0008 I-1 on Solana: a stranger flushed the session's 25 USDC in two parts (10 + 15), so the
// program charged the fixed fee twice. The job records both settlements, alerts
// flush_fixed_fee_excess and keeps the extra (0.10) on its details.
func TestStrangerFlushesInPartsAlertTheExtraFixedFee(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	if _, err := e.snd.Plan(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := forwarder(t, e.job(id))
	stranger := solfake.Key("stranger")
	ix, err := f.Flush(stranger, usdc, source, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, part := range [][2]uint64{{150_000, 9_850_000}, {175_000, 14_825_000}} {
		inner := []solrpc.Instruction{solfake.Transfer(source, ata(t, treasury()), pda, part[0]),
			solfake.Transfer(source, ata(t, merchant()), pda, part[1])}
		tx := solfake.Tx(solfake.Signature("partial flush "+string(rune('a'+i))), stranger, []solrpc.Instruction{solfake.Raw(ix)},
			map[int][]solrpc.Instruction{0: inner}, nil, nil)
		e.cl.Finalize(e.cl.Mine(tx))
	}
	e.cl.CloseAccount(source)

	e.step()
	j := e.job(id)
	if j.Status != store.JobDone || j.Outcome != "settled_elsewhere" {
		t.Fatalf("job %+v", j)
	}
	if st := e.settlements(id); len(st) != 2 {
		t.Fatalf("settlements %+v", st)
	}
	if j.Details["extra_fixed_fee"] != "100000" || j.Details["flush_settlements"] != "2" {
		t.Fatalf("job details %v", j.Details)
	}
	if !e.alerts.Has(flushfee.AlertKind) {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
}
