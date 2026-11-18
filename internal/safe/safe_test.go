package safe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/alert"
)

func TestStepTurnsAPanicIntoAnAlertedError(t *testing.T) {
	var logs bytes.Buffer
	rec := &alert.Recorder{}

	n, err := Step(t.Context(), rec, slog.New(slog.NewTextHandler(&logs, nil)), "sender:base",
		func(context.Context) (int, error) { panic("bad row") })

	var p *PanicError
	if n != 0 || !errors.As(err, &p) || p.Value != "bad row" || err.Error() != "panic: bad row" {
		t.Fatalf("n %d err %v", n, err)
	}
	if !strings.Contains(logs.String(), "step panicked") || !strings.Contains(logs.String(), "loop=sender:base") ||
		!strings.Contains(logs.String(), "safe_test.go") {
		t.Fatalf("log has no loop name or stack: %s", logs.String())
	}
	all := rec.All()
	if len(all) != 1 || all[0].Kind != "step_panicked" || all[0].Severity != alert.Critical || all[0].Chain != "base" ||
		all[0].Fields["loop"] != "sender:base" || all[0].Fields["panic"] != "bad row" {
		t.Fatalf("alerts %+v", all)
	}
}

func TestStepPassesResultsThrough(t *testing.T) {
	rec := &alert.Recorder{}
	boom := errors.New("boom")

	n, err := Step(t.Context(), rec, nil, "webhooks", func(context.Context) (int, error) { return 3, boom })

	if n != 3 || err != boom || len(rec.All()) != 0 {
		t.Fatalf("n %d err %v alerts %v", n, err, rec.Kinds())
	}
}

func TestCallRecoversWithoutAlerting(t *testing.T) {
	err := Call(func() error { panic(errors.New("nil deref")) })

	var p *PanicError
	if !errors.As(err, &p) || len(p.Stack) == 0 {
		t.Fatalf("err %v", err)
	}
	if err := Call(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
