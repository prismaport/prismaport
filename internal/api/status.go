package api

import (
	"fmt"
	"net/http"

	"github.com/prismaport/prismaport/internal/service"
)

// httpStatus is the HTTP status of each service error kind: the one place a kind becomes a
// status. An unknown kind is a server error.
func httpStatus(k service.Kind) int {
	switch k {
	case service.KindInvalid:
		return http.StatusBadRequest
	case service.KindUnauthorized:
		return http.StatusUnauthorized
	case service.KindForbidden:
		return http.StatusForbidden
	case service.KindNotFound:
		return http.StatusNotFound
	case service.KindConflict:
		return http.StatusConflict
	case service.KindGone:
		return http.StatusGone
	case service.KindRateLimited:
		return http.StatusTooManyRequests
	case service.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// transportError is a refusal about the HTTP request itself, with a status no service kind
// names (405, 413, 415); its body is the wrapped error's.
type transportError struct {
	status int
	err    *service.Error
}

func (e *transportError) Error() string { return fmt.Sprintf("%d %s", e.status, e.err.Error()) }

func (e *transportError) Unwrap() error { return e.err }

// refuse is a transport refusal with status and an invalid_request body.
func refuse(status int, msg string) error {
	return &transportError{status: status, err: &service.Error{Kind: service.KindInvalid, Code: service.CodeInvalidRequest, Message: msg}}
}
