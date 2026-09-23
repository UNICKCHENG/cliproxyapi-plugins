package main

import (
	"context"
	"strings"
)

type hostCallbackIDKey struct{}

// withHostCallbackID stores the host's callback id on the request context so later host.http
// and host.log calls can restore the client's cancellation and request-id.
func withHostCallbackID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, hostCallbackIDKey{}, id)
}

func hostCallbackIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(hostCallbackIDKey{}).(string)
	return strings.TrimSpace(id)
}
