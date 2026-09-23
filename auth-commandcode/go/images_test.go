package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestParseFetchableImageURLRejectsPrivate(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/x.png",
		"http://localhost/x.png",
		"http://192.168.1.4/x.png",
		"http://10.0.0.1/x.png",
		"http://169.254.169.254/latest",
		"http://user:pass@example.com/x.png",
		"ftp://example.com/x.png",
	}
	for _, ref := range cases {
		if _, errParse := parseFetchableImageURL(ref); errParse == nil {
			t.Errorf("%s: expected rejection", ref)
		}
	}
}

func TestParseDataURL(t *testing.T) {
	data, mimeType, ok, errParse := parseDataURL("data:image/png;base64,aGVsbG8=")
	if errParse != nil || !ok || mimeType != "image/png" || data == "" {
		t.Fatalf("parse = %q %q %v %v", data, mimeType, ok, errParse)
	}
	if _, _, _, errBad := parseDataURL("data:application/pdf;base64,aaaa"); errBad == nil {
		t.Fatal("expected mime rejection")
	}
}

func restoreImageDialHooks(t *testing.T) {
	t.Helper()
	previousLookup := lookupImageIPs
	previousDial := dialImageNetwork
	t.Cleanup(func() {
		lookupImageIPs = previousLookup
		dialImageNetwork = previousDial
	})
}

func TestDialSafeImagePinsResolvedPublicIP(t *testing.T) {
	restoreImageDialHooks(t)
	lookupImageIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("169.254.169.254"), net.ParseIP("1.1.1.1")}, nil
	}
	var dialed []string
	dialImageNetwork = func(_ context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, network+" "+address)
		return nil, fmt.Errorf("stop")
	}
	_, errDial := dialSafeImage(context.Background(), "tcp", "evil.example:443")
	if errDial == nil {
		t.Fatal("expected dial to fail after pinning")
	}
	if len(dialed) != 1 || dialed[0] != "tcp 1.1.1.1:443" {
		t.Fatalf("dialed = %#v", dialed)
	}
}

func TestDialSafeImageRejectsPrivateResolution(t *testing.T) {
	restoreImageDialHooks(t)
	lookupImageIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1"), net.ParseIP("169.254.169.254")}, nil
	}
	dialed := 0
	dialImageNetwork = func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, fmt.Errorf("should not dial")
	}
	_, errDial := dialSafeImage(context.Background(), "tcp", "evil.example:80")
	if errDial == nil {
		t.Fatal("expected private resolution to be rejected")
	}
	if dialed != 0 {
		t.Fatalf("dialed private address %d times", dialed)
	}
	if _, _, errDownload := downloadImage(context.Background(), "", "http://evil.example/x.png"); errDownload == nil {
		t.Fatal("expected download of rebound private host to fail")
	}
	if dialed != 0 {
		t.Fatalf("download dialed private address %d times", dialed)
	}
}

func TestDownloadImageDialsPinnedIPNotHostname(t *testing.T) {
	restoreImageDialHooks(t)
	lookupImageIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	var dialed []string
	dialImageNetwork = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, fmt.Errorf("stop")
	}
	_, _, errDownload := downloadImage(context.Background(), "", "https://cdn.example/pic.png")
	if errDownload == nil {
		t.Fatal("expected download to fail from stubbed dial")
	}
	if len(dialed) != 1 || !strings.HasPrefix(dialed[0], "8.8.8.8:") {
		t.Fatalf("dialed hostname instead of pinned IP: %#v", dialed)
	}
}

func TestCheckImageRedirectRejectsLoopback(t *testing.T) {
	original, errNew := http.NewRequest(http.MethodGet, "https://cdn.example/pic.png", nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	next, errNext := http.NewRequest(http.MethodGet, "http://127.0.0.1/secret.png", nil)
	if errNext != nil {
		t.Fatal(errNext)
	}
	if errCheck := checkImageRedirect(next, []*http.Request{original}); errCheck == nil {
		t.Fatal("expected loopback redirect to be rejected")
	}
}
