// Package apperror defines structured application errors with machine-readable
// codes, safe user-facing messages, and optional internal detail for logging.
package apperror

import (
	"errors"
	"net/http"
)

// Code is a machine-readable error identifier sent to clients.
type Code string

const (
	CodeAgentNotFound        Code = "AGENT_NOT_FOUND"
	CodeInvalidRequest       Code = "INVALID_REQUEST"
	CodeDAGValidationFailed  Code = "DAG_VALIDATION_FAILED"
	CodePlannerFailed        Code = "PLANNER_FAILED"
	CodeTaskFailed           Code = "TASK_FAILED"
	CodeMaxIterationsReached Code = "MAX_ITERATIONS_REACHED"
	CodeHITLRejected         Code = "HITL_REJECTED"
	CodeTimeout              Code = "TIMEOUT"
	CodeInternal             Code = "INTERNAL_ERROR"
)

// AppError is a structured error with a code, a safe user-facing message,
// and an optional internal detail that must never be sent to clients.
type AppError struct {
	Code       Code   // machine-readable, sent to client
	Message    string // safe user-facing description
	Detail     string // internal detail — logged server-side only
	HTTPStatus int    // status to use on non-streaming HTTP responses
}

func (e *AppError) Error() string { return string(e.Code) + ": " + e.Message }

// New creates an AppError.
func New(code Code, message string, httpStatus int) *AppError {
	return &AppError{Code: code, Message: message, HTTPStatus: httpStatus}
}

// Wrap creates an AppError and attaches the original error as internal detail.
func Wrap(code Code, message string, httpStatus int, cause error) *AppError {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return &AppError{Code: code, Message: message, Detail: detail, HTTPStatus: httpStatus}
}

// As extracts an *AppError from err if present.
func As(err error) (*AppError, bool) {
	var ae *AppError
	ok := errors.As(err, &ae)
	return ae, ok
}

// Classify maps unstructured internal errors (from the DAG / planner / evaluator)
// to structured AppErrors. Call this at the handler boundary before sending
// anything to the client.
func Classify(err error) *AppError {
	if err == nil {
		return nil
	}
	// Already classified — return as-is.
	if ae, ok := As(err); ok {
		return ae
	}

	msg := err.Error()

	switch {
	case contains(msg, "exceeded max_iterations"):
		return Wrap(CodeMaxIterationsReached,
			"The agent reached its iteration limit without a satisfactory answer. Please rephrase your request.",
			http.StatusUnprocessableEntity, err)

	case contains(msg, "DAG validation failed"):
		return Wrap(CodeDAGValidationFailed,
			"The execution plan contained an invalid step. Please try again.",
			http.StatusUnprocessableEntity, err)

	case contains(msg, "planner failed", "planner returned empty"):
		return Wrap(CodePlannerFailed,
			"The planner could not generate an execution plan. Please try again.",
			http.StatusBadGateway, err)

	case contains(msg, "agent not found", "agent \""):
		return Wrap(CodeAgentNotFound,
			"The requested agent does not exist.",
			http.StatusNotFound, err)

	case contains(msg, "context deadline exceeded", "context canceled"):
		return Wrap(CodeTimeout,
			"The request timed out. Please try again.",
			http.StatusGatewayTimeout, err)

	case contains(msg, "human rejected", "hitl"):
		return Wrap(CodeHITLRejected,
			"A required approval was rejected.",
			http.StatusForbidden, err)

	case contains(msg, "all tasks failed", "task failed"):
		return Wrap(CodeTaskFailed,
			"One or more tasks failed to complete.",
			http.StatusBadGateway, err)

	default:
		return Wrap(CodeInternal,
			"An unexpected error occurred. Please try again.",
			http.StatusInternalServerError, err)
	}
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
