package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

type stubHostHTTPStream struct {
	status  int
	headers http.Header
	chunks  [][]byte
	opens   int
	reads   int
	closes  int
}

func (s *stubHostHTTPStream) Open(_ context.Context, req rpcHostHTTPRequest) (rpcHostHTTPStreamResponse, error) {
	s.opens++
	return rpcHostHTTPStreamResponse{StatusCode: s.status, Headers: s.headers, StreamID: "img-1"}, nil
}

func (s *stubHostHTTPStream) Read(_ context.Context, streamID string) (rpcHostHTTPStreamReadResponse, error) {
	s.reads++
	if s.reads-1 >= len(s.chunks) {
		return rpcHostHTTPStreamReadResponse{Done: true}, nil
	}
	payload := s.chunks[s.reads-1]
	done := s.reads >= len(s.chunks)
	return rpcHostHTTPStreamReadResponse{Payload: payload, Done: done}, nil
}

func (s *stubHostHTTPStream) Close(streamID string) error {
	s.closes++
	return nil
}

func TestParseFetchableImageURLRejectsPrivateDestinations(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		want string
	}{
		{name: "loopback", url: "http://127.0.0.1/secret", want: "not allowed"},
		{name: "ipv6 loopback", url: "http://[::1]/secret", want: "not allowed"},
		{name: "file", url: "file:///etc/passwd", want: "scheme"},
		{name: "userinfo", url: "https://user:pass@example.com/a.png", want: "credentials"},
		{name: "private ipv4", url: "http://10.0.0.1/a.png", want: "not allowed"},
		{name: "link local", url: "http://169.254.169.254/latest", want: "not allowed"},
		{name: "cgnat", url: "http://100.64.0.1/a.png", want: "not allowed"},
		{name: "localhost name", url: "http://localhost/a.png", want: "not allowed"},
		{name: "metadata host", url: "http://metadata.google.internal/", want: "not allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, errURL := parseFetchableImageURL(test.url)
			if errURL == nil {
				t.Fatal("expected the URL to be rejected")
			}
			if !strings.Contains(errURL.Error(), test.want) {
				t.Errorf("error = %q, want %q", errURL, test.want)
			}
			if strings.Contains(errURL.Error(), "/secret") || strings.Contains(errURL.Error(), "passwd") {
				t.Errorf("error leaked the URL path: %q", errURL)
			}
		})
	}
}

func TestDownloadImageClosesTheHostStream(t *testing.T) {
	stub := &stubHostHTTPStream{
		status:  http.StatusOK,
		headers: http.Header{"Content-Type": []string{"image/png"}},
		chunks:  [][]byte{[]byte("PNG")},
	}
	previous := hostHTTPStream
	hostHTTPStream = stub
	t.Cleanup(func() { hostHTTPStream = previous })

	data, mime, errDownload := downloadImage(context.Background(), "https://1.1.1.1/a.png")
	if errDownload != nil {
		t.Fatalf("downloadImage: %v", errDownload)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if data != base64.StdEncoding.EncodeToString([]byte("PNG")) {
		t.Errorf("data = %q", data)
	}
	if stub.closes != 1 {
		t.Errorf("closes = %d, want 1", stub.closes)
	}
}

func TestDownloadImageStopsMidStreamWhenOverLimit(t *testing.T) {
	previousMax := imageMaxBytes
	imageMaxBytes = 10
	t.Cleanup(func() { imageMaxBytes = previousMax })

	stub := &stubHostHTTPStream{
		status: http.StatusOK,
		chunks: [][]byte{
			make([]byte, 8),
			make([]byte, 8),
		},
	}
	previous := hostHTTPStream
	hostHTTPStream = stub
	t.Cleanup(func() { hostHTTPStream = previous })

	_, _, errDownload := downloadImage(context.Background(), "https://1.1.1.1/a.png")
	if errDownload == nil || !strings.Contains(errDownload.Error(), "byte limit") {
		t.Fatalf("error = %v, want the byte limit", errDownload)
	}
	if stub.closes != 1 {
		t.Errorf("closes = %d, want 1 after refusing the oversize body", stub.closes)
	}
	if stub.reads < 2 {
		t.Errorf("reads = %d, want the second chunk to trigger the limit", stub.reads)
	}
}

func TestDownloadImageClosesAfterUnexpectedStatus(t *testing.T) {
	stub := &stubHostHTTPStream{status: http.StatusNotFound}
	previous := hostHTTPStream
	hostHTTPStream = stub
	t.Cleanup(func() { hostHTTPStream = previous })

	_, _, errDownload := downloadImage(context.Background(), "https://1.1.1.1/a.png")
	if errDownload == nil {
		t.Fatal("expected a non-2xx status to fail")
	}
	if stub.closes != 1 {
		t.Errorf("closes = %d, want 1 after the status error", stub.closes)
	}
	if strings.Contains(errDownload.Error(), "/a.png") {
		t.Errorf("error leaked the path: %q", errDownload)
	}
}
