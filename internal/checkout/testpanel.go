package checkout

import (
	"context"
	"slices"

	"github.com/prismaport/prismaport/internal/service"
)

// TestPanelActions are the simulator actions the hosted checkout offers on a test-mode session,
// so whoever is integrating can finish a flow from the browser the way test cards do elsewhere.
var TestPanelActions = []string{service.SimPayExact, service.SimPayPartial, service.SimOverpay, service.SimExpire, service.SimSettle}

// TestPanelEvent is one event produced by a test panel step, reduced to its type and session version.
type TestPanelEvent struct {
	Type           string `json:"type"`
	SessionVersion int64  `json:"session_version"`
}

// TestPanelResult is the payer-side answer: the events the step produced (types only), whether
// the session now has a settlement, and the public session.
type TestPanelResult struct {
	Action  string           `json:"action"`
	Events  []TestPanelEvent `json:"events"`
	Settled bool             `json:"settled"`
	Session PublicSession    `json:"session"`
}

// Simulate runs one simulator step on a test-mode session for the hosted checkout. The session
// id is the only scope, as for every public endpoint; a live session is refused before anything
// else happens, and only the panel's actions are allowed.
func (c *Checkout) Simulate(ctx context.Context, id string, in service.SimulateInput) (TestPanelResult, error) {
	ses, err := c.load(ctx, id)
	if err != nil {
		return TestPanelResult{}, err
	}
	if ses.Livemode {
		return TestPanelResult{}, errf(service.KindForbidden, service.CodeTestModeOnly, "The test panel works on test payments only.")
	}
	if !slices.Contains(TestPanelActions, in.Action) {
		return TestPanelResult{}, errf(service.KindInvalid, service.CodeInvalidRequest,
			"action must be one of pay_exact, pay_partial, overpay, expire or settle")
	}
	res, err := c.svc.SimulateFromTestPanel(ctx, ses.MerchantID, id, in)
	if err != nil {
		return TestPanelResult{}, err
	}
	out := TestPanelResult{Action: res.Action, Events: []TestPanelEvent{}, Settled: res.Session.Settlement != nil}
	for _, e := range res.Events {
		out.Events = append(out.Events, TestPanelEvent{Type: e.Type, SessionVersion: e.SessionVersion})
	}
	if out.Session, err = c.Session(ctx, id, false); err != nil {
		return TestPanelResult{}, err
	}
	return out, nil
}
