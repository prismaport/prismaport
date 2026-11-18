package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhook"
)

// MountSandbox adds the test-mode developer tools: "Send test event" and the lifecycle simulator
// on the merchant API and the dashboard, and the hosted inspector inbox (its public receiving URL
// and its dashboard routes).
// Every one of them refuses live mode. d sends the test events with the same client
// and network rules as the worker's deliveries.
func (s *Server) MountSandbox(d *webhook.Dispatcher) {
	limiter := s.publicLimiter()
	sendTest := func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		m, err := readFields(r, "type")
		if err != nil {
			return err
		}
		typ, err := decodeField[string](m, "type", "an event type string")
		if err != nil {
			return err
		}
		if typ == nil {
			empty := ""
			typ = &empty
		}
		if err := service.RequireTestMode(a); err != nil {
			return err
		}
		if err := limiter.allowKey(w, r, RouteWebhookTest, a.MerchantID); err != nil {
			return err
		}
		res, err := d.SendTest(r.Context(), a, r.PathValue("id"), *typ)
		return respond(w, res, err)
	}
	s.route("POST /v1/webhook_endpoints/{id}/test", apikey.WebhooksWrite, sendTest)
	s.mode("POST", "/webhook_endpoints/{id}/test", sendTest)

	simulate := func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		if err := service.RequireTestMode(a); err != nil {
			return err
		}
		in, err := simulateInput(r)
		if err != nil {
			return err
		}
		res, err := s.svc.Simulate(r.Context(), a, r.PathValue("id"), in)
		return respond(w, res, err)
	}
	s.route("POST /v1/test_helpers/sessions/{id}/simulate", apikey.SessionsWrite, simulate)
	s.mode("POST", "/sessions/{id}/simulate", simulate)
	s.mux.HandleFunc("POST "+service.InboxPath+"{token}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := s.receiveInbox(w, r, limiter); err != nil {
			s.writeError(w, r, err)
		}
	})
	s.mux.HandleFunc(service.InboxPath+"{token}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		s.writeError(w, r, refuse(http.StatusMethodNotAllowed, "an inbox only receives POST requests; read them in the dashboard"))
	})
	s.mode("GET", "/sandbox/inbox", reply(func(r *http.Request, a service.Auth) (any, error) {
		i, err := s.svc.SandboxInbox(r.Context(), a)
		return map[string]any{"inbox": i}, err
	}))
	s.mode("POST", "/sandbox/inbox", reply(func(r *http.Request, a service.Auth) (any, error) {
		i, err := s.svc.CreateSandboxInbox(r.Context(), a)
		return map[string]any{"inbox": i}, err
	}))
	s.mode("DELETE", "/sandbox/inbox", reply(func(r *http.Request, a service.Auth) (any, error) {
		return map[string]any{"deleted": true}, s.svc.DeleteSandboxInbox(r.Context(), a)
	}))
	s.mode("GET", "/sandbox/inbox/requests", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.SandboxRequests(r.Context(), a, p.cursor, p.limit)
	}))
}

// receiveInbox stores a POST addressed to a hosted inbox. The token in the URL is the only
// credential; the body is kept as received, up to the size limit.
func (s *Server) receiveInbox(w http.ResponseWriter, r *http.Request, limiter *publicLimiter) error {
	inbox, err := s.svc.LookupSandboxInbox(r.Context(), r.PathValue("token"))
	if err != nil {
		return err
	}
	if err := limiter.allowKey(w, r, RouteSandboxInbox, inbox.ID); err != nil {
		return err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, service.InboxMaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return refuse(http.StatusRequestEntityTooLarge, "the inbox accepts bodies up to 64 KB")
		}
		return badRequest("could not read request body")
	}
	id, err := s.svc.ReceiveSandboxRequest(r.Context(), inbox, r.Method, r.Header, body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"received": true, "id": id})
}
