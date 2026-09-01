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
	// An unspecified code is not a classification, and the bridge does send one: a key Cursor
	// rejects arrives as UNAUTHENTICATED carrying SDK_ERROR_CODE_UNSPECIFIED. Treating that as
	// a branchable code would park a dead credential as if it were a transient fault.
	if details, ok := sdkErrorDetails(err); ok && details.GetSdkErrorCode() != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED {
		return failureFromDetails(details)
	}
	return failureFromTransport(err)
}

// failureFromTransport classifies an error the bridge reported without a usable sdk_error_code.
//
// The Connect code is coarser than sdk_error_code and cannot distinguish a fault of the request
// from one of the credential, which is why it is only consulted once the detail has turned out to
// carry nothing. The mappings are the standard gRPC ones; what makes them worth having is that
// the host's handling of 401, 403 and 429 differs from its handling of an unclassified failure,
// and an unclassified rejected key would cool the credential instead of retiring it.
func failureFromTransport(err error) upstreamFailure {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return upstreamFailure{Message: err.Error()}
	}
	message := transportMessage(err, connectErr)
	switch connectErr.Code() {
	case connect.CodeUnauthenticated:
		// Not the bridge's own bearer token: that one is proved by the Ping the handshake
		// ends with, and it does not change while the process lives.
		return upstreamFailure{Message: upstreamErrorText(http.StatusUnauthorized, message), HTTPStatus: http.StatusUnauthorized}
	case connect.CodePermissionDenied:
		return upstreamFailure{Message: upstreamErrorText(http.StatusForbidden, message), HTTPStatus: http.StatusForbidden}
	case connect.CodeResourceExhausted:
		return upstreamFailure{
			Message:    upstreamErrorText(http.StatusTooManyRequests, message),
			HTTPStatus: http.StatusTooManyRequests,
			Retryable:  true,
		}
	case connect.CodeDeadlineExceeded, connect.CodeUnavailable:
		// A bridge that finished its handshake answers or fails fast, so these mean it is
		// stuck reaching Cursor. Reported verbatim that reads as if the plugin hung, which
		// sends operators looking in the wrong place; the proxy is what they need to check,
		// because the bridge reaches Cursor through it.
		//
		// It stays unclassified: no status is right for a network outage, which affects every
		// credential equally rather than discrediting this one.
		return upstreamFailure{
			Message: fmt.Sprintf(
				"could not reach the cursor api (%s): check that the plugin's proxy-url is set and can reach %s",
				message, cursorBackendHost,
			),
			Retryable: true,
		}
	default:
		// Nothing here says whether the credential, the request or Cursor is at fault, so the
		// error is passed on whole rather than given a status that would misdirect the host.
		return upstreamFailure{Message: err.Error()}
	}
}

// transportMessage picks the most specific text available. The bridge leaves sdk_error_code
// unspecified while still describing the failure, and that description is what an operator acts
// on.
func transportMessage(err error, connectErr *connect.Error) string {
	if details, ok := sdkErrorDetails(err); ok {
		if message := strings.TrimSpace(details.GetMessage()); message != "" {
			return message
		}
	}
	if message := strings.TrimSpace(connectErr.Message()); message != "" {
		return message
	}
	return err.Error()
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
	return requestFault("run_failed", message)
}

// runFailureFromResult classifies a terminal non-success run. A run that reached this point
// already authenticated: CreateAgent and Send both succeeded, so the failure is request-scoped
// unless the result itself carries a credential or quota code.
func runFailureFromResult(result *sdkv1.RunStreamResult, statusMessage string) upstreamFailure {
	if details, ok := sdkErrorDetailsFromRun(result, statusMessage); ok {
		return failureFromDetails(details)
	}
	return runFailure(runFailureMessage(result, statusMessage))
}

func sdkErrorDetailsFromRun(result *sdkv1.RunStreamResult, statusMessage string) (*sdkv1.SdkErrorDetails, bool) {
	if result == nil {
		return nil, false
	}
	code := strings.TrimSpace(result.GetErrorCode())
	if code == "" {
		return nil, false
	}
	key := strings.ToUpper(code)
	if !strings.HasPrefix(key, "SDK_ERROR_CODE_") {
		key = "SDK_ERROR_CODE_" + key
	}
	mapped, ok := sdkv1.SdkErrorCode_value[key]
	if !ok || mapped == 0 {
		return nil, false
	}
	return &sdkv1.SdkErrorDetails{
		SdkErrorCode: sdkv1.SdkErrorCode(mapped),
		Message:      runFailureMessage(result, statusMessage),
	}, true
}
