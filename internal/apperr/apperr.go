// Package apperr defines the product's stable error model.
//
// Error codes are part of the public API contract: clients branch on Code,
// never on HTTP status or message text. Retryable is an explicit field —
// HTTP 409 can be retryable (optimistic-lock conflict) or not (illegal
// state transition), so it must never be inferred from the status code.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

type Code string

const (
	InvalidRequest     Code = "invalid_request"     // 400, client input rejected
	AuthFailed         Code = "auth_failed"         // 401, missing/invalid/expired credential
	Forbidden          Code = "forbidden"           // 403, authenticated but not allowed
	NotFound           Code = "not_found"           // 404
	Conflict           Code = "conflict"            // 409, illegal state transition or concurrent write
	QuotaExceeded      Code = "quota_exceeded"      // 402/429-adjacent; billing-shaped rejection
	RateLimited        Code = "rate_limited"        // 429
	BackendUnavailable Code = "backend_unavailable" // 503, runtime backend unreachable
	Unimplemented      Code = "unimplemented"       // 501, refused rather than silently degraded
	Internal           Code = "internal"            // 500
)

// HTTPStatus maps a code to its outer HTTP status. The mapping is advisory:
// clients must read Code, but correct transport semantics still matter.
func (c Code) HTTPStatus() int {
	switch c {
	case InvalidRequest:
		return http.StatusBadRequest
	case AuthFailed:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case QuotaExceeded:
		return http.StatusPaymentRequired
	case RateLimited:
		return http.StatusTooManyRequests
	case Unimplemented:
		return http.StatusNotImplemented
	case BackendUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// E is the canonical error type. Handlers may attach request IDs at the edge.
type E struct {
	Code      Code
	Message   string
	Retryable bool
	// RetryAfter, when > 0, is a server suggestion in seconds (rate limits).
	RetryAfter int
	// Details optionally carries structured, non-sensitive context.
	Details map[string]any
}

func (e *E) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code Code, message string) *E {
	return &E{Code: code, Message: message}
}

// Retryable marks err as safe to retry (transport-level idempotency hint).
func Retryable(err error, msg string) *E {
	return &E{Code: Internal, Message: msg, Retryable: true}
}

func Invalid(format string, args ...any) *E {
	return &E{Code: InvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func NotFoundErr(what string) *E {
	return &E{Code: NotFound, Message: what + " not found"}
}

func ConflictErr(format string, args ...any) *E {
	return &E{Code: Conflict, Message: fmt.Sprintf(format, args...)}
}

func InternalErr(format string, args ...any) *E {
	return &E{Code: Internal, Message: fmt.Sprintf(format, args...), Retryable: true}
}

func Unavailable(format string, args ...any) *E {
	return &E{Code: BackendUnavailable, Message: fmt.Sprintf(format, args...), Retryable: true}
}

func Unimpl(format string, args ...any) *E {
	return &E{Code: Unimplemented, Message: fmt.Sprintf(format, args...)}
}

func Auth(msg string) *E {
	return &E{Code: AuthFailed, Message: msg}
}

func ForbiddenErr(msg string) *E {
	return &E{Code: Forbidden, Message: msg}
}

func RateLimit(retryAfter int) *E {
	return &E{Code: RateLimited, Message: "rate limit exceeded", RetryAfter: retryAfter}
}

func Quota(format string, args ...any) *E {
	return &E{Code: QuotaExceeded, Message: fmt.Sprintf(format, args...)}
}

// From extracts an *E, wrapping unknown errors as retryable internal errors.
// Unknown errors are wrapped verbatim so logs keep the root cause. A nil
// error yields nil — callers checking code on a nil error is a bug, but a
// panic in an error helper would take the daemon down with it.
func From(err error) *E {
	if err == nil {
		return nil
	}
	var e *E
	if errors.As(err, &e) {
		return e
	}
	return &E{Code: Internal, Message: err.Error(), Retryable: true}
}
