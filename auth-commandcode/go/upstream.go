package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	providerPathPrefix     = "/provider/v1"
	catalogTimeout         = 30 * time.Second
	nonStreamTimeout       = 10 * time.Minute
	defaultRequestMaxBytes = 20 << 20
)

func providerEndpoint(base, suffix string) (string, error) {
	normalized, errBase := normalizeAPIBase(base)
	if errBase != nil {
		return "", errBase
	}
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		return "", fmt.Errorf("provider endpoint path is empty")
	}
	if strings.HasSuffix(normalized, providerPathPrefix) {
		return normalized + "/" + suffix, nil
	}
	joined, errJoin := url.JoinPath(normalized, providerPathPrefix, suffix)
	if errJoin != nil {
		return "", fmt.Errorf("build provider endpoint: %w", errJoin)
	}
	return joined, nil
}

type upstreamCall struct {
	Method    string
	URL       string
	APIKey    string
	ProxyURL  string
	Body      []byte
	Accept    string
	ZDR       bool
	Timeout   time.Duration
	MaxBytes  int
	Stream    bool
	ClientZDR bool
}

func doJSON(ctx context.Context, call upstreamCall) (int, http.Header, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if call.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, call.Timeout)
		defer cancel()
	}
	resp, errDo := doHTTP(ctx, call)
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	defer resp.Body.Close()
	maxBytes := call.MaxBytes
	if maxBytes <= 0 {
		maxBytes = upstreamResponseLimit
	}
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if errRead != nil {
		if ctx.Err() != nil {
			return resp.StatusCode, resp.Header, nil, ctx.Err()
		}
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("read upstream response: %w", errRead)
	}
	if len(body) > maxBytes {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("upstream response exceeds %d bytes", maxBytes)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func doHTTP(ctx context.Context, call upstreamCall) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(call.Body) > defaultRequestMaxBytes {
		return nil, requestFault(fmt.Sprintf("request exceeds %d bytes", defaultRequestMaxBytes))
	}
	var body io.Reader
	if len(call.Body) > 0 {
		body = bytes.NewReader(call.Body)
	}
	req, errNew := http.NewRequestWithContext(ctx, call.Method, call.URL, body)
	if errNew != nil {
		return nil, fmt.Errorf("create upstream request: %w", errNew)
	}
	req.Header = upstreamHeaders(call)
	resp, errTrip := roundTrip(ctx, call.ProxyURL, req)
	if errTrip != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errTrip
	}
	if resp == nil {
		return nil, fmt.Errorf("upstream returned no response")
	}
	return resp, nil
}

func upstreamHeaders(call upstreamCall) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+call.APIKey)
	if call.Accept != "" {
		header.Set("Accept", call.Accept)
	} else {
		header.Set("Accept", "application/json")
	}
	if len(call.Body) > 0 {
		header.Set("Content-Type", "application/json")
	}
	header.Set("User-Agent", userAgent())
	if call.ZDR && !call.ClientZDR {
		header.Set("x-cmd-zdr", "1")
	}
	return header
}

func clientRequestedZDR(headers http.Header) bool {
	if headers == nil {
		return false
	}
	value := strings.TrimSpace(headers.Get("x-cmd-zdr"))
	return value == "1" || strings.EqualFold(value, "true")
}
