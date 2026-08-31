package main

import (
	"encoding/base64"
	"testing"
)

func TestDecodeSidecarHTTPBody(t *testing.T) {
	t.Parallel()

	decoded, errDecode := decodeSidecarHTTPBody(base64.StdEncoding.EncodeToString([]byte("payload")))
	if errDecode != nil {
		t.Fatalf("decodeSidecarHTTPBody() error = %v", errDecode)
	}
	if string(decoded) != "payload" {
		t.Fatalf("body = %q, want payload", decoded)
	}

	empty, errEmpty := decodeSidecarHTTPBody("")
	if errEmpty != nil {
		t.Fatalf("decodeSidecarHTTPBody(empty) error = %v", errEmpty)
	}
	if len(empty) != 0 {
		t.Fatalf("empty body = %q, want nil/empty", empty)
	}
}

func TestCloneHTTPHeader(t *testing.T) {
	t.Parallel()

	source := map[string][]string{"Authorization": {"Bearer token"}}
	cloned := cloneHTTPHeader(source)
	cloned["Authorization"][0] = "changed"
	if source["Authorization"][0] != "Bearer token" {
		t.Fatalf("clone mutated source header")
	}
}
