package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// upstreamFailure describes a failed call in the terms the host classifies on.
type upstreamFailure struct {
	// Message is the text the host receives. It is a JSON error body whenever the
	// classification has to survive a channel that carries no status code.
	Message string
	// HTTPStatus is the status the host attributes to the failure, or zero when unknown.
	HTTPStatus int
	// Retryable reports whether Cursor marked the failure as worth retrying.
	Retryable bool
}

// upstreamError carries an upstreamFailure through the error interface so the classification
// survives the ordinary error returns between the run driver and the executor entry points.
type upstreamError struct {
	failure upstreamFailure
}

func (e *upstreamError) Error() string { return e.failure.Message }

// failureFrom recovers the classification from any error the bridge path can produce.
func failureFrom(err error) upstreamFailure {
	if err == nil {
		return upstreamFailure{Message: "cursor request failed"}
	}
	var classified *upstreamError
	if errors.As(err, &classified) {
		return classified.failure
	}
	if details, ok := sdkErrorDetails(err); ok {
		return failureFromDetails(details)
	}
	return upstreamFailure{Message: err.Error()}
}

// sdkErrorDetails pulls the structured detail the bridge attaches to a failed RPC. Branching on
// sdk_error_code is the only stable way to classify: the transport code is coarser and the
// message is free-form.
func sdkErrorDetails(err error) (*sdkv1.SdkErrorDetails, bool) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return nil, false
	}
	for _, detail := range connectErr.Details() {
		value, errValue := detail.Value()
		if errValue != nil {
			continue
		}
		if details, ok := value.(*sdkv1.SdkErrorDetails); ok {
			return details, true
		}
	}
	return nil, false
}

// requestFaultCodes name the failures that are properties of the request rather than of the
// credential: a model the account cannot reach, a feature its plan excludes, a malformed option.
//
// Reporting them without a status makes the host park the whole credential, so one model the
// account cannot reach would take down every model it can reach.
var requestFaultCodes = map[sdkv1.SdkErrorCode]string{
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL:       "model_not_available",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_PLAN_REQUIRED:       "plan_required",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_FEATURE_UNAVAILABLE: "feature_unavailable",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR:    "validation_error",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_BRANCH_NAME: "validation_error",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_REPOSITORY_REQUIRED: "validation_error",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_RUN_NOT_CANCELLABLE: "validation_error",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_AGENT_NOT_FOUND:     "validation_error",
	sdkv1.SdkErrorCode_SDK_ERROR_CODE_RUN_NOT_FOUND:       "validation_error",
}

// failureFromDetails maps a bridge error code onto the status the host reasons about.
//
// SdkErrorDetails carries no HTTP status, but the host's classification is expressed in statuses:
// 401 retires the credential, 429 backs it off, 400 blames the request, and no status at all
// lands in the generic branch that cools the credential.
func failureFromDetails(details *sdkv1.SdkErrorDetails) upstreamFailure {
	message := strings.TrimSpace(details.GetMessage())
	if message == "" {
		message = "cursor request failed"
	}
	if requestID := strings.TrimSpace(details.GetRequestId()); requestID != "" {
		// Cursor support traces on the full request ID, so it is logged rather than truncated.
		hostLog("warn", "cursor request failed", map[string]any{
			"request_id": requestID,
			"sdk_code":   details.GetSdkErrorCode().String(),
		})
	}
	retryable := details.GetRetryAfter() != nil

	if code, isRequestFault := requestFaultCodes[details.GetSdkErrorCode()]; isRequestFault {
		return requestFault(code, message)
	}
	switch details.GetSdkErrorCode() {
	case sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED, sdkv1.SdkErrorCode_SDK_ERROR_CODE_API_KEY_NOT_FOUND:
		return upstreamFailure{Message: upstreamErrorText(http.StatusUnauthorized, message), HTTPStatus: http.StatusUnauthorized}
	case sdkv1.SdkErrorCode_SDK_ERROR_CODE_ROLE_FORBIDDEN:
		return upstreamFailure{Message: upstreamErrorText(http.StatusForbidden, message), HTTPStatus: http.StatusForbidden}
	case sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED:
		return upstreamFailure{
			Message:    upstreamErrorText(http.StatusTooManyRequests, rateLimitText(message, details)),
			HTTPStatus: http.StatusTooManyRequests,
			Retryable:  retryable,
		}
	default:
		// Deliberately unclassified: AGENT_BUSY, UPSTREAM_ERROR, INTERNAL_ERROR and any code
		// added to sdk.v1 later describe a transient or unknown condition, and inventing a
		// status for them would misdirect the host's credential handling.
		return upstreamFailure{Message: message, Retryable: retryable}
	}
}

// rateLimitText appends the wait Cursor suggested, which is the one piece of a rate-limit reply
// an operator can act on.
func rateLimitText(message string, details *sdkv1.SdkErrorDetails) string {
	retryAfter := details.GetRetryAfter()
	if retryAfter == nil {
		return message
	}
	return fmt.Sprintf("%s (retry after %s)", message, retryAfter.AsDuration())
}

func upstreamErrorText(status int, message string) string {
	return fmt.Sprintf("cursor upstream error %d: %s", status, message)
}

// requestFault renders a 400 whose body carries the classification.
//
// The body is JSON because the streaming path can only carry text: the host reads error.type out
// of the body when no status is available.
func requestFault(code, message string) upstreamFailure {
	body, errMarshal := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    code,
			"message": message,
		},
	})
	if errMarshal != nil {
		return upstreamFailure{Message: message, HTTPStatus: http.StatusBadRequest}
	}
	return upstreamFailure{Message: string(body), HTTPStatus: http.StatusBadRequest}
}

// modelUnavailableMarkers identify a Cursor refusal to serve one model to this account: region
// restrictions and plan or team policy.
//
// A run that fails inside the agent reports free-form text rather than an SdkErrorCode, so these
// markers are the only signal that the failure belongs to the request and not the credential.
var modelUnavailableMarkers = []string{
	"not supported in your region",
	"not available in your region",
	"model not available",
	"model is not available",
	"model not supported",
	"model is not supported",
	"not available on your plan",
	"not available for your plan",
}

// runFailure classifies a run that ended in a non-terminal-success state.
//
// The terminal RunStreamResult.error_code can be empty for a failed run, in which case the
// human-readable reason only appears in the last status message on the stream.
func runFailure(message string) upstreamFailure {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "cursor run failed"
	}
	lower := strings.ToLower(message)
	for _, marker := range modelUnavailableMarkers {
		if strings.Contains(lower, marker) {
			return requestFault("model_not_available", message)
		}
	}
	return upstreamFailure{Message: message}
}
