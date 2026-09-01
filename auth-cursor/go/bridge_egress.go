package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// cursorBackendHost answers the Connect RPCs the SDK's backend client makes. Its paths are
// "/<proto package>.<Service>/<Method>", the shape the SDK's createMethodUrl builds.
const cursorBackendHost = "api2.cursor.sh"

// cursorCloudHost answers the SDK's cloud REST client, whose paths are "/v1/agents...".
//
// The two clients read the same CURSOR_BACKEND_URL but default to different hosts, so redirecting
// that variable collapses both onto one listener and the split has to be reconstructed here.
const cursorCloudHost = "api.cursor.com"

// The upstream targets are variables so a test can point them at a local server instead of
// Cursor. Nothing at runtime rewrites them.
var (
	cursorBackendTarget = url.URL{Scheme: "https", Host: cursorBackendHost}
	cursorCloudTarget   = url.URL{Scheme: "https", Host: cursorCloudHost}
)

// bridgeEgress is the loopback listener one bridge's Cursor traffic is redirected through.
//
// The bridge issues its backend calls on bun's node:http default Agent, which ignores the proxy
// environment variables, so a configured proxy cannot simply be handed to the child process.
// Pointing CURSOR_BACKEND_URL at this listener instead moves the outbound hop into Go, where the
// proxy is honoured. Serving plain HTTP also pins the bridge to HTTP/1.1 -- its own rule is that
// a plain-http backend URL never gets HTTP/2 -- so this needs no h2c support.
type bridgeEgress struct {
	server  *http.Server
	baseURL string
}

// egressProxyEnvVars are the host variables an egress falls back to when the plugin was given no
// proxy of its own, in the order net/http consults them.
var egressProxyEnvVars = []string{
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
	"ALL_PROXY", "all_proxy",
}

// resolveEgressProxy picks the proxy this egress dials through: the resolved proxyURL first, then
// whatever the host environment already uses, so a machine that only reaches the internet through
// a proxy keeps working without plugin configuration.
func resolveEgressProxy(proxyURL string) string {
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		return trimmed
	}
	for _, name := range egressProxyEnvVars {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// egressProxySchemes are the first-hop schemes net/http can dial. socks5 is included because
// Transport dials it natively.
var egressProxySchemes = map[string]struct{}{
	"http":    {},
	"https":   {},
	"socks5":  {},
	"socks5h": {},
}

// parseEgressProxy validates the configured proxy before a bridge is started, so a typo is
// reported at startup instead of as an unexplained failure on the first Cursor call.
func parseEgressProxy(proxy string) (*url.URL, error) {
	// A bare host:port is a common way to write this, and net/http's own environment parsing
	// applies the same fixup.
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	parsed, errParse := url.Parse(proxy)
	if errParse != nil {
		return nil, fmt.Errorf("parse cursor proxy %q: %w", proxy, errParse)
	}
	if _, supported := egressProxySchemes[strings.ToLower(parsed.Scheme)]; !supported {
		return nil, fmt.Errorf("cursor proxy %q uses unsupported scheme %q, want http, https or socks5", proxy, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("cursor proxy %q carries no host", proxy)
	}
	return parsed, nil
}

// egressListenAddrs are tried in order.
//
// The IPv6 loopback is preferred only because the SDK turns off TLS certificate verification for
// the whole bridge process when its backend URL matches "localhost" or "127.0.0.1" literally.
// That would weaken the connections the bridge still makes directly, and there is no reason to
// accept it just to reach a listener on the same machine.
var egressListenAddrs = []string{"[::1]:0", "127.0.0.1:0"}

func listenEgress() (net.Listener, error) {
	var first error
	for _, addr := range egressListenAddrs {
		listener, errListen := net.Listen("tcp", addr)
		if errListen == nil {
			return listener, nil
		}
		if first == nil {
			first = errListen
		}
	}
	return nil, fmt.Errorf("listen for cursor egress on loopback: %w", first)
}

// startBridgeEgress brings up the egress for a bridge, or reports that none is needed.
//
// A nil egress means the bridge should reach Cursor itself: with no proxy to apply, a
// pass-through hop would add a failure mode and buy nothing.
func startBridgeEgress(proxyURL string) (*bridgeEgress, error) {
	proxy := resolveEgressProxy(proxyURL)
	if proxy == "" {
		return nil, nil
	}
	parsed, errParse := parseEgressProxy(proxy)
	if errParse != nil {
		return nil, errParse
	}
	listener, errListen := listenEgress()
	if errListen != nil {
		return nil, errListen
	}
	address := listener.Addr().String()
	if !strings.HasPrefix(address, "[") {
		hostLog("warn", "cursor egress bound to the IPv4 loopback", map[string]any{
			"detail": "the bridge disables TLS certificate verification for its own direct connections when its backend URL is 127.0.0.1, and the IPv6 loopback was unavailable",
		})
	}

	server := &http.Server{
		Handler: &httputil.ReverseProxy{
			Rewrite:   routeCursorRequest,
			Transport: egressTransport(parsed),
			// Agent output arrives as stream frames that have to reach the bridge as they
			// are produced, and the default buffering would batch them.
			FlushInterval: -1,
			// A request sent to the wrong Cursor host is not a transport failure -- it arrives
			// and comes back 404 -- so ErrorHandler never sees it. Recording where a rejected
			// request was routed is what makes that case diagnosable in one step.
			ModifyResponse: func(response *http.Response) error {
				if response.StatusCode >= http.StatusBadRequest && response.Request != nil {
					hostLog("debug", "cursor egress upstream rejected a request", map[string]any{
						"host":   response.Request.URL.Host,
						"path":   response.Request.URL.Path,
						"status": response.StatusCode,
					})
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, errProxy error) {
				// Without this the outbound failure is invisible: the bridge just never
				// answers and the plugin reports a deadline.
				hostLog("warn", "cursor egress request failed", map[string]any{
					"host":  r.URL.Host,
					"path":  r.URL.Path,
					"error": errProxy.Error(),
				})
				w.WriteHeader(http.StatusBadGateway)
			},
		},
		// The only client is the bridge on loopback, so this just bounds a stuck child.
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if errServe := server.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			hostLog("warn", "cursor egress stopped", map[string]any{"error": errServe.Error()})
		}
	}()
	return &bridgeEgress{server: server, baseURL: "http://" + address}, nil
}

func egressTransport(proxy *url.URL) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyURL(proxy),
		// The SDK picks HTTP/1.1 for its own backend calls under bun. Keeping 1.1 upstream too
		// makes this hop transparent rather than an upgrade, and avoids having to translate
		// HTTP/2 trailers back down for the bridge.
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Deliberately no ResponseHeaderTimeout: a run holds its stream open for as long as it
		// takes, and the plugin imposes no bound of its own on that.
	}
}

// cursorCloudPathPrefix covers the whole of the cloud client's surface: every path it builds is
// under /v1/ (agents, me, models, repositories).
//
// It is the exception rather than the rule, so it is what gets matched. The backend is the
// default because that is what CURSOR_BACKEND_URL means when nothing overrides it, and because
// the backend's surface is the open-ended one: Connect RPCs at /<package>.<Service>/<Method>,
// but also plain REST like /auth/exchange_user_api_key, which the SDK calls before its first
// request to trade the API key for an access token.
const cursorCloudPathPrefix = "/v1/"

// routeCursorRequest sends a request on to the Cursor host the SDK would have used for it.
//
// One environment variable redirects two clients that default to different hosts, so the split
// has to be reconstructed. Getting it wrong is not a connection failure: the request arrives at
// the wrong Cursor host and comes back 404.
func routeCursorRequest(request *httputil.ProxyRequest) {
	target := cursorBackendTarget
	if strings.HasPrefix(request.In.URL.Path, cursorCloudPathPrefix) {
		target = cursorCloudTarget
	}
	// SetURL also rewrites the Host header, which upstream needs for routing and SNI.
	request.SetURL(&target)
}

// stop drops the listener and every connection on it. The bridge that was using it is already
// gone by the time this runs, so there is nothing left to drain.
func (e *bridgeEgress) stop() {
	if e == nil {
		return
	}
	_ = e.server.Close()
}
