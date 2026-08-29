package provider

import (
	"errors"
	"fmt"
)

// ErrorCategory is the normalized classification of a provider failure.
//
// Adapters must map transport- and vendor-specific failures onto these so the
// rest of Boop can react without knowing which backend produced the error.
type ErrorCategory string

const (
	ErrUnavailable           ErrorCategory = "unavailable"
	ErrTimeout               ErrorCategory = "timeout"
	ErrAuthentication        ErrorCategory = "authentication"
	ErrRateLimited           ErrorCategory = "rate_limited"
	ErrInvalidRequest        ErrorCategory = "invalid_request"
	ErrUnsupportedCapability ErrorCategory = "unsupported_capability"
	ErrMalformedResponse     ErrorCategory = "malformed_response"
	ErrServer                ErrorCategory = "server_error"
	ErrCancelled             ErrorCategory = "cancelled"
)

// Error is a normalized provider failure.
//
// Message is safe to show to a user. Detail holds implementation specifics and
// should only surface in debug mode.
type Error struct {
	Category ErrorCategory
	Provider string
	Model    string
	Message  string
	Detail   string
	// Status is the HTTP status where one applies, otherwise zero.
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("%s: %s: %s", e.Provider, e.Category, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether retrying the same request could plausibly succeed.
func (e *Error) Retryable() bool {
	switch e.Category {
	case ErrUnavailable, ErrTimeout, ErrRateLimited, ErrServer:
		return true
	default:
		return false
	}
}

// NewError builds a normalized provider error.
func NewError(category ErrorCategory, providerName, message string, cause error) *Error {
	return &Error{Category: category, Provider: providerName, Message: message, Err: cause}
}

// CategoryOf extracts the normalized category from err, reporting ok=false when
// err is not a provider error.
func CategoryOf(err error) (ErrorCategory, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Category, true
	}
	return "", false
}

// IsRetryable reports whether err is a provider error worth retrying.
func IsRetryable(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Retryable()
}

// UnsupportedCapabilityError describes a task that the selected model cannot
// perform, carrying the missing capabilities so the UI can suggest alternatives.
type UnsupportedCapabilityError struct {
	Provider string
	Model    string
	Missing  []Capability
}

func (e *UnsupportedCapabilityError) Error() string {
	return fmt.Sprintf("model %s/%s lacks required capabilities: %v", e.Provider, e.Model, e.Missing)
}
