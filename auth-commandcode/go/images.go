package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var imageMaxBytes = 20 << 20

const defaultImageMimeType = "image/png"

var (
	metadataImageHosts = map[string]struct{}{
		"metadata.google.internal": {},
		"metadata.goog":            {},
	}
	cgnatNet = func() *net.IPNet {
		_, block, _ := net.ParseCIDR("100.64.0.0/10")
		return block
	}()
	allowedImageMIME = map[string]struct{}{
		"image/jpeg": {},
		"image/jpg":  {},
		"image/png":  {},
		"image/gif":  {},
		"image/webp": {},
	}
)

func parseDataURL(reference string) (data, mimeType string, ok bool, err error) {
	reference = strings.TrimSpace(reference)
	if !strings.HasPrefix(strings.ToLower(reference), "data:") {
		return "", "", false, nil
	}
	payload := reference[len("data:"):]
	meta, encoded, found := strings.Cut(payload, ",")
	if !found {
		return "", "", true, fmt.Errorf("image data URL is not valid")
	}
	mimeType = defaultImageMimeType
	if media := strings.TrimSpace(strings.Split(meta, ";")[0]); media != "" {
		mimeType = strings.ToLower(media)
	}
	if errMIME := validateImageMIME(mimeType); errMIME != nil {
		return "", "", true, errMIME
	}
	raw := strings.TrimSpace(encoded)
	decoded, errDecode := base64.StdEncoding.DecodeString(raw)
	if errDecode != nil {
		decoded, errDecode = base64.RawStdEncoding.DecodeString(raw)
	}
	if errDecode != nil {
		return "", "", true, fmt.Errorf("image data URL is not valid base64")
	}
	if len(decoded) == 0 {
		return "", "", true, fmt.Errorf("image data URL is empty")
	}
	if len(decoded) > imageMaxBytes {
		return "", "", true, fmt.Errorf("image exceeds the %d byte limit", imageMaxBytes)
	}
	return base64.StdEncoding.EncodeToString(decoded), mimeType, true, nil
}

func downloadImage(ctx context.Context, proxyURL, reference string) (string, string, error) {
	parsed, errURL := parseFetchableImageURL(reference)
	if errURL != nil {
		return "", "", errURL
	}
	label := imageURLLabel(parsed)
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if errNew != nil {
		return "", "", fmt.Errorf("download image %s: %w", label, errNew)
	}
	client, errClient := imageHTTPClient(proxyURL)
	if errClient != nil {
		return "", "", errClient
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("download image %s: %w", label, errDo)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("download image %s: unexpected status %d", label, resp.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, int64(imageMaxBytes)+1))
	if errRead != nil {
		return "", "", fmt.Errorf("download image %s: %w", label, errRead)
	}
	if len(body) > imageMaxBytes {
		return "", "", fmt.Errorf("download image %s exceeds the %d byte limit", label, imageMaxBytes)
	}
	if len(body) == 0 {
		return "", "", fmt.Errorf("download image %s: empty response", label)
	}
	mimeType := responseMimeType(resp.Header)
	if mimeType == "" {
		mimeType = defaultImageMimeType
	}
	if errMIME := validateImageMIME(mimeType); errMIME != nil {
		return "", "", fmt.Errorf("download image %s: %w", label, errMIME)
	}
	return base64.StdEncoding.EncodeToString(body), mimeType, nil
}

func validateImageMIME(mimeType string) error {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if _, ok := allowedImageMIME[mimeType]; ok {
		return nil
	}
	return fmt.Errorf("image MIME type %s is not allowed", mimeType)
}

func checkImageRedirect(next *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("download image: too many redirects")
	}
	if next == nil || next.URL == nil {
		return fmt.Errorf("download image: redirect target is not allowed")
	}
	if _, errNext := parseFetchableImageURL(next.URL.String()); errNext != nil {
		return fmt.Errorf("download image: redirect target is not allowed")
	}
	return nil
}

func imageHTTPClient(proxyURL string) (*http.Client, error) {
	transport := imageTransport()
	if strings.TrimSpace(proxyURL) != "" {
		proxy, errProxy := url.Parse(strings.TrimSpace(proxyURL))
		if errProxy != nil {
			return nil, fmt.Errorf("proxy-url is not a valid URL")
		}
		transport.Proxy = http.ProxyURL(proxy)
		transport.DialContext = nil
	} else {
		transport.Proxy = nil
		transport.DialContext = dialSafeImage
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: checkImageRedirect,
		Transport:     transport,
	}, nil
}

func imageTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		return base.Clone()
	}
	return &http.Transport{}
}

var lookupImageIPs = defaultLookupImageIPs
var dialImageNetwork = defaultDialImageNetwork

func defaultLookupImageIPs(ctx context.Context, host string) ([]net.IP, error) {
	addrs, errLookup := net.DefaultResolver.LookupIPAddr(ctx, host)
	if errLookup != nil {
		return nil, errLookup
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP != nil {
			ips = append(ips, addr.IP)
		}
	}
	return ips, nil
}

func defaultDialImageNetwork(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

func dialSafeImage(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		return nil, fmt.Errorf("image address is not valid")
	}
	if blockedImageHost(host) {
		return nil, fmt.Errorf("image host %s is not allowed", host)
	}
	ips, errLookup := resolveImageIPs(ctx, host)
	if errLookup != nil || len(ips) == 0 {
		return nil, fmt.Errorf("image host %s could not be resolved", host)
	}
	var lastErr error
	attempted := false
	for _, ip := range ips {
		if blockedImageIP(ip) {
			continue
		}
		attempted = true
		conn, errDial := dialImageNetwork(ctx, network, net.JoinHostPort(ip.String(), port))
		if errDial == nil {
			return conn, nil
		}
		lastErr = errDial
	}
	if !attempted {
		return nil, fmt.Errorf("image host %s is not allowed", host)
	}
	return nil, fmt.Errorf("download image: %w", lastErr)
}

func resolveImageIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return lookupImageIPs(ctx, host)
}

func parseFetchableImageURL(reference string) (*url.URL, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, fmt.Errorf("image URL is empty")
	}
	parsed, errParse := url.Parse(reference)
	if errParse != nil {
		return nil, fmt.Errorf("image URL is not valid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		if scheme == "" {
			return nil, fmt.Errorf("image URL scheme is not allowed")
		}
		return nil, fmt.Errorf("image URL scheme %s is not allowed", scheme)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("image URL must not include credentials")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return nil, fmt.Errorf("image URL has no host")
	}
	if blockedImageHost(host) {
		return nil, fmt.Errorf("image host %s is not allowed", host)
	}
	return parsed, nil
}

func imageURLLabel(parsed *url.URL) string {
	if parsed == nil {
		return "image"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func blockedImageHost(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" {
		return true
	}
	if _, blocked := metadataImageHosts[lower]; blocked {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return blockedImageIP(ip)
	}
	return false
}

func blockedImageIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnatNet.Contains(v4) {
		return true
	}
	return false
}

func responseMimeType(headers http.Header) string {
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		return ""
	}
	mimeType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return ""
	}
	return strings.ToLower(mimeType)
}
