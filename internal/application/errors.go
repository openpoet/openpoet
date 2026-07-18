package application

import "errors"

type ErrorKind string

const (
	ErrorValidation ErrorKind = "validation"
	ErrorNotFound   ErrorKind = "not_found"
	ErrorConflict   ErrorKind = "conflict"
)

type Error struct {
	Kind    ErrorKind
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func ErrorIsKind(err error, kind ErrorKind) bool {
	var appErr *Error
	return errors.As(err, &appErr) && appErr.Kind == kind
}

// ErrorCode returns the stable machine-readable code of an application error
// (e.g. "action_approval_required"), or "" when err is not an *Error. Transport
// layers surface it so callers get a structured signal, not just prose.
func ErrorCode(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func validationError(code, message string) error {
	return &Error{Kind: ErrorValidation, Code: code, Message: message}
}

func notFoundError(code, message string, cause error) error {
	return &Error{Kind: ErrorNotFound, Code: code, Message: message, Cause: cause}
}

func conflictError(code, message string) error {
	return &Error{Kind: ErrorConflict, Code: code, Message: message}
}
