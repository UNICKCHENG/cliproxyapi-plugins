package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
	"github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1/sdkv1connect"
)

// startToolCallback listens on loopback and registers the URL with the bridge so CallCustomTool
// RPCs come back to this process. Failure is fatal: a tool-enabled bridge without a callback
// would silently run text-only.
func startToolCallback(p *bridgeProcess) error {
	listener, errListen := listenToolCallback()
	if errListen != nil {
		return errListen
	}
	tokenBytes := make([]byte, 32)
	if _, errRand := rand.Read(tokenBytes); errRand != nil {
		_ = listener.Close()
		return fmt.Errorf("generate cursor tool callback token: %w", errRand)
	}
	token := hex.EncodeToString(tokenBytes)

	mux := http.NewServeMux()
	mux.Handle(sdkv1connect.NewSdkCustomToolCallbackServiceHandler(
		toolCallbackServer{},
		connect.WithInterceptors(callbackBearerAuth(token)),
	))
	server := &http.Server{
		Handler: mux,
		// Callbacks block until the Agent CLI returns a tool result, so there is no body or
		// write timeout. ReadHeaderTimeout only bounds a stuck handshake.
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if errServe := server.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			hostLog("warn", "cursor tool callback stopped", map[string]any{"error": errServe.Error()})
		}
	}()

	url := "http://" + listener.Addr().String()
	p.callbackSrv = server
	p.callbackURL = url
	p.callbackToken = token

	ctx, cancel := context.WithTimeout(context.Background(), bridgeStartupTimeout)
	defer cancel()
	if _, errSet := p.control.SetToolCallback(ctx, connect.NewRequest(&sdkv1.SetToolCallbackRequest{
		Url:       url,
		AuthToken: token,
	})); errSet != nil {
		_ = server.Close()
		p.callbackSrv = nil
		p.callbackURL = ""
		p.callbackToken = ""
		return fmt.Errorf("register cursor tool callback: %w", errSet)
	}
	return nil
}

func listenToolCallback() (net.Listener, error) {
	var first error
	for _, addr := range toolCallbackListenAddrs {
		listener, errListen := net.Listen("tcp", addr)
		if errListen == nil {
			return listener, nil
		}
		if first == nil {
			first = errListen
		}
	}
	return nil, fmt.Errorf("listen for cursor tool callback on loopback: %w", first)
}

func stopToolCallback(p *bridgeProcess) {
	if p == nil || p.callbackSrv == nil {
		return
	}
	_ = p.callbackSrv.Close()
	p.callbackSrv = nil
}

// callbackBearerAuth rejects CallCustomTool RPCs that do not present the per-process token.
type callbackBearerAuth string

func (a callbackBearerAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		if !bearerMatches(request.Header().Get("Authorization"), string(a)) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
		}
		return next(ctx, request)
	}
}

func (a callbackBearerAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a callbackBearerAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func bearerMatches(got, token string) bool {
	want := "Bearer " + token
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		return true
	}
	return false
}
