// Package safe turns a panic in a background step into an error, so bad data in one row costs
// only the step that read it, not the whole process.
package safe

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"github.com/prismaport/prismaport/internal/alert"
)

// PanicError is a recovered panic: its value and the stack of the goroutine that panicked.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Call runs f and returns a panic in it as a *PanicError. It neither logs nor alerts: it is for
// helper goroutines whose caller handles the error (a provider call in a quorum read, say).
func Call(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return f()
}

// Step runs one step of the loop called name (a chain's loops are "<kind>:<chain id>"). A panic
// in it is logged with its stack, raised as a Critical step_panicked alert and returned as a
// *PanicError, which the caller counts as a failed step.
func Step(ctx context.Context, alerts alert.Sink, log *slog.Logger, name string, step func(context.Context) (int, error)) (n int, err error) {
	err = Call(func() error {
		var err error
		n, err = step(ctx)
		return err
	})
	if p, ok := err.(*PanicError); ok {
		Report(ctx, alerts, log, name, p)
		return 0, err
	}
	return n, err
}

// Report logs a recovered panic of the loop called name with its stack and raises
// step_panicked. A nil log discards the log line; the alert is still raised.
func Report(ctx context.Context, alerts alert.Sink, log *slog.Logger, name string, p *PanicError) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log.Error("step panicked", "loop", name, "panic", fmt.Sprint(p.Value), "stack", string(p.Stack))
	if alerts == nil {
		return
	}
	_, chain, _ := strings.Cut(name, ":")
	alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: chain, Kind: "step_panicked",
		Message: name + " step panicked; the step was abandoned and the loop goes on",
		Fields:  map[string]string{"loop": name, "panic": truncate(fmt.Sprint(p.Value), 300)}})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
