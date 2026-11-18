package api

import (
	"encoding/json"
	"net/http"

	"github.com/prismaport/prismaport/internal/service"
)

// Most handlers read their input, call one service method and answer 200 with its result. The
// adapters below keep that shape in one place; a handler that answers another status or sets
// headers is written out.

// respond answers 200 with v, unless err is set.
func respond(w http.ResponseWriter, v any, err error) error {
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// markReplayed sets Idempotent-Replayed on the answer to a replayed idempotent request.
func markReplayed(w http.ResponseWriter, replayed bool) {
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
}

// reply adapts a handler that returns its 200 body.
func reply(f func(r *http.Request, a service.Auth) (any, error)) handler {
	return func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		v, err := f(r, a)
		return respond(w, v, err)
	}
}

// listPage is the page a list request asks for: the cursor query parameter and the page size
// (pageLimit).
type listPage struct {
	cursor string
	limit  int
}

// listRoute adapts a paginated list. The page size is checked before f runs.
func listRoute(f func(r *http.Request, a service.Auth, p listPage) (any, error)) handler {
	return func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		limit, err := pageLimit(r)
		if err != nil {
			return err
		}
		v, err := f(r, a, listPage{cursor: r.URL.Query().Get("cursor"), limit: limit})
		return respond(w, v, err)
	}
}

// jsonRoute adapts a handler that takes the request body decoded by decode and returns its 200
// body.
func jsonRoute[In any](decode func(body []byte) (In, error), f func(r *http.Request, a service.Auth, in In) (any, error)) handler {
	return func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		body, err := readBody(r)
		if err != nil {
			return err
		}
		in, err := decode(body)
		if err != nil {
			return err
		}
		v, err := f(r, a, in)
		return respond(w, v, err)
	}
}

// createRoute is jsonRoute for an idempotent create: f also takes the Idempotency-Key and the raw
// body (the service fingerprints the request with it), and says whether it answered a replay.
func createRoute[In any](decode func(body []byte) (In, error),
	f func(r *http.Request, a service.Auth, key string, body []byte, in In) (any, bool, error)) handler {
	return func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		body, err := readBody(r)
		if err != nil {
			return err
		}
		in, err := decode(body)
		if err != nil {
			return err
		}
		v, replayed, err := f(r, a, r.Header.Get("Idempotency-Key"), body, in)
		if err != nil {
			return err
		}
		markReplayed(w, replayed)
		return writeJSON(w, http.StatusOK, v)
	}
}

// mode registers a dashboard route scoped to the mode parameter (modeRoute) for a handler that
// needs only the mode's Auth, not the dashboard session.
func (s *Server) mode(method, path string, h handler) {
	s.modeRoute(method, path, func(w http.ResponseWriter, r *http.Request, _ service.DashSession, a service.Auth) error {
		return h(w, r, a)
	})
}

// field decodes one optional member of a JSON object: whether it was sent, whether as null, and
// its value otherwise. kind completes the message of a value of the wrong type.
func field[T any](m map[string]json.RawMessage, name, kind string) (service.Field[T], error) {
	raw, ok := m[name]
	if !ok {
		return service.Field[T]{}, nil
	}
	if isNull(raw) {
		return service.Field[T]{Set: true, Null: true}, nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return service.Field[T]{}, badRequest("%s must be %s", name, kind)
	}
	return service.Set(v), nil
}

// decodeField is field for a member where null is the same as leaving it out: nil then.
func decodeField[T any](m map[string]json.RawMessage, name string, kind string) (*T, error) {
	f, err := field[T](m, name, kind)
	return f.Ptr(), err
}

// requiredStringFields decodes the string members names of m into dst, in order; each is required.
func requiredStringFields(m map[string]json.RawMessage, names []string, dst ...*string) error {
	for i, name := range names {
		v, err := decodeField[string](m, name, "a string")
		if err != nil {
			return err
		}
		if v == nil {
			return badRequest("%s is required", name)
		}
		*dst[i] = *v
	}
	return nil
}

// optionalStringFields decodes the string members names of m into dst, in order; a member left
// out or null leaves its dst as it is.
func optionalStringFields(m map[string]json.RawMessage, names []string, dst ...*string) error {
	for i, name := range names {
		v, err := decodeField[string](m, name, "a string")
		if err != nil {
			return err
		}
		if v != nil {
			*dst[i] = *v
		}
	}
	return nil
}

// members decodes the members of one JSON object in turn and keeps the first error: once a
// member fails, the rest are left zero, so the request fails on the first bad member as if each
// decode were checked.
type members struct {
	m   map[string]json.RawMessage
	err error
}

// member decodes one member with field unless an earlier one failed.
func member[T any](d *members, name, kind string) service.Field[T] {
	if d.err != nil {
		return service.Field[T]{}
	}
	f, err := field[T](d.m, name, kind)
	d.err = err
	return f
}
