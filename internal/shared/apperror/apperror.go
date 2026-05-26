// Package apperror defines a transport-agnostic error type. The domain layer
// builds errors with these constructors; the HTTP layer maps Kind to a status
// code. This keeps business logic free of any knowledge about HTTP.
package apperror

import "errors"

// Kind classifies an error so the transport layer can map it to a protocol
// status without importing every module's domain package.
type Kind int

const (
	KindInternal Kind = iota
	KindNotFound
	KindInvalid
	KindConflict
)

// Error is the shared application error.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches errors by Kind and Msg so package-level sentinels keep working
// with errors.Is even after the error has been wrapped.
func (e *Error) Is(target error) bool {
	var t *Error
	ok := errors.As(target, &t)
	return ok && t.Kind == e.Kind && t.Msg == e.Msg
}

func mk(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg} }

func Internal(msg string) *Error { return mk(KindInternal, msg) }
func NotFound(msg string) *Error { return mk(KindNotFound, msg) }
func Invalid(msg string) *Error  { return mk(KindInvalid, msg) }
func Conflict(msg string) *Error { return mk(KindConflict, msg) }

// Wrap returns an internal error carrying the underlying cause. Use it for
// unexpected infrastructure failures (DB, network) that should surface as 500.
func Wrap(msg string, cause error) *Error {
	return &Error{Kind: KindInternal, Msg: msg, Err: cause}
}
