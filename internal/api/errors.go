package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
)

// ErrorResponse is the single, consistent JSON error envelope every endpoint
// returns for non-2xx responses. It mirrors vidra-core's ErrorResponse exactly
// so a caller can rely on one shape across both services.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries a stable machine-readable code, a human-readable message,
// the request id, and (for validation failures) the offending fields.
type ErrorBody struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	RequestID string       `json:"request_id,omitempty"`
	Fields    []FieldError `json:"fields,omitempty"`
}

// FieldError is one field-level validation problem.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError renders as 422 unprocessable_entity with field detail.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string { return "validation failed" }

// newValidation builds a single-field ValidationError.
func newValidation(field, message string) *ValidationError {
	return &ValidationError{Fields: []FieldError{{Field: field, Message: message}}}
}

// httpErrorHandler is Echo's central error handler. It converts any error into
// the ErrorResponse envelope, logs 5xx with context, and never leaks internal
// error text to clients on a 500.
func (s *Server) httpErrorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	status := http.StatusInternalServerError
	message := "an unexpected error occurred"
	code := ""
	var fields []FieldError

	var he *echo.HTTPError
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		status = http.StatusUnprocessableEntity
		message = "validation failed"
		code = "unprocessable_entity"
		fields = ve.Fields
	case errors.As(err, &he):
		status = he.Code
		if he.Message != nil {
			message = fmt.Sprintf("%v", he.Message)
		}
		if he.Internal != nil {
			err = he.Internal
		}
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
		message = "the request timed out"
		code = "request_timeout"
	}

	reqID := c.Response().Header().Get(echo.HeaderXRequestID)

	if status >= http.StatusInternalServerError {
		s.logger.Error("request failed",
			"error", err,
			"method", c.Request().Method,
			"path", c.Path(),
			"status", status,
			"request_id", reqID,
		)
		if code == "" {
			message = "an unexpected error occurred"
		}
	}

	if code == "" {
		code = codeForStatus(status)
	}

	resp := ErrorResponse{Error: ErrorBody{Code: code, Message: message, RequestID: reqID, Fields: fields}}
	var writeErr error
	if c.Request().Method == http.MethodHead {
		writeErr = c.NoContent(status)
	} else {
		writeErr = c.JSON(status, resp)
	}
	if writeErr != nil {
		s.logger.Error("failed to write error response", "error", writeErr, "request_id", reqID)
	}
}

// writeError sends the envelope directly (used by middleware that must respond
// before the handler chain, e.g. HMAC auth failures).
func writeError(c echo.Context, status int, code, message string) error {
	reqID := c.Response().Header().Get(echo.HeaderXRequestID)
	return c.JSON(status, ErrorResponse{Error: ErrorBody{Code: code, Message: message, RequestID: reqID}})
}

// codeForStatus maps an HTTP status to a stable, snake_case error code.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusRequestEntityTooLarge:
		return "request_entity_too_large"
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusInternalServerError:
		return "internal_error"
	}
	switch {
	case status >= 500:
		return "server_error"
	case status >= 400:
		return "client_error"
	default:
		return "error"
	}
}
