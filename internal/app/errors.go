package app

import "fmt"

// Error is a safe, stable error shape for CLI and API adapters. Implementing
// services should use it for expected user-facing failures.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func NewError(code, message string, status int) *Error {
	if code == "" {
		code = "internal_error"
	}
	if message == "" {
		message = code
	}
	if status == 0 {
		status = 500
	}
	return &Error{Code: code, Message: message, Status: status}
}

func NotImplemented(operation string) *Error {
	if operation == "" {
		operation = "this operation"
	}
	return NewError("not_implemented", fmt.Sprintf("%s is not implemented yet", operation), 501)
}

func Invalid(message string) *Error {
	return NewError("invalid_request", message, 400)
}
