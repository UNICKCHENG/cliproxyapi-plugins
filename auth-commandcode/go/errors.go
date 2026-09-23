package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	errorSummaryLimit     = 400
	upstreamResponseLimit = 32 << 20
)

type upstreamFailure struct {
	Message    string
	HTTPStatus int
	Retryable  bool
}

type upstreamError struct {
	failure upstreamFailure
}

func (e *upstreamError) Error() string {
	if e == nil {
		return "commandcode request failed"
	}
	return e.failure.Message
}

func requestFault(message string) *upstreamError {
	return &upstreamError{failure: upstreamFailure{
		Message:    openaiErrorJSON(http.StatusBadRequest, "invalid_request_error", message),
		HTTPStatus: http.StatusBadRequest,
	}}
}

func incompleteStreamError() *upstreamError {
	return &upstreamError{failure: upstreamFailure{
		Message:    openaiErrorJSON(http.StatusBadGateway, "server_error", "upstream stream ended before completion"),
		HTTPStatus: http.StatusBadGateway,
		Retryable:  true,
	}}
}

func failureFrom(err error) upstreamFailure {
	if err == nil {
		return upstreamFailure{Message: "commandcode request failed"}
	}
	var classified *upstreamError
	if errors.As(err, &classified) {
		return classified.failure
	}
	if errors.Is(err, context.Canceled) {
		return upstreamFailure{Message: "request canceled", HTTPStatus: 499}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return upstreamFailure{
			Message:    "request timed out",
			HTTPStatus: http.StatusGatewayTimeout,
			Retryable:  true,
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return upstreamFailure{
			Message:    "upstream timed out",
			HTTPStatus: http.StatusGatewayTimeout,
			Retryable:  true,
		}
	}
	message := err.Error()
	if errors.Is(err, io.EOF) {
		message = "upstream closed the connection"
	}
	return upstreamFailure{Message: message, Retryable: true}
}

func classifyHTTPStatus(status int, body []byte) upstreamFailure {
	code, errType, message := parseUpstreamError(body)
	if message == "" {
		message = strings.TrimSpace(http.StatusText(status))
		if message == "" {
			message = "upstream request failed"
		}
	}
	retryable := status == http.StatusTooManyRequests || status >= 500
	if status == http.StatusUnauthorized {
		retryable = false
	}
	if status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusUnprocessableEntity {
		retryable = false
	}
	if errType == "" {
		errType = defaultErrorType(status)
	}
	if code == "" && status == http.StatusForbidden {
		code = "upgrade_required"
	}
	return upstreamFailure{
		Message:    openaiErrorJSON(status, errType, messageWithCode(code, message)),
		HTTPStatus: status,
		Retryable:  retryable,
	}
}

func defaultErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "invalid_request_error"
	}
}

func parseUpstreamError(body []byte) (code, errType, message string) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", "", sanitizeErrorText(string(body))
	}
	errorNode := gjson.GetBytes(body, "error")
	if errorNode.IsObject() {
		message = firstNonEmpty(
			errorNode.Get("message").String(),
			gjson.GetBytes(body, "message").String(),
		)
		errType = firstNonEmpty(errorNode.Get("type").String(), errorNode.Get("code").String())
		code = errorNode.Get("code").String()
		return strings.TrimSpace(code), strings.TrimSpace(errType), sanitizeErrorText(message)
	}
	if errorNode.Type == gjson.String {
		return "", "", sanitizeErrorText(errorNode.String())
	}
	return "", "", sanitizeErrorText(gjson.GetBytes(body, "message").String())
}

func messageWithCode(code, message string) string {
	code = strings.TrimSpace(code)
	message = strings.TrimSpace(message)
	if code == "" || strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
		return message
	}
	if message == "" {
		return code
	}
	return code + ": " + message
}

func openaiErrorJSON(status int, errType, message string) string {
	if errType == "" {
		errType = defaultErrorType(status)
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": sanitizeErrorText(message),
			"type":    errType,
		},
	})
	if errMarshal != nil {
		return `{"error":{"message":"upstream request failed","type":"server_error"}}`
	}
	return string(payload)
}

func sanitizeErrorText(message string) string {
	message = strings.TrimSpace(message)
	message = strings.ReplaceAll(message, "\n", " ")
	if message == "" {
		return ""
	}
	runes := []rune(message)
	if len(runes) > errorSummaryLimit {
		message = string(runes[:errorSummaryLimit]) + "..."
	}
	return message
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
