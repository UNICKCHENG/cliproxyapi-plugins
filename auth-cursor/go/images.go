package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// imageMaxBytes caps a downloaded attachment. Cursor rejects oversized images anyway, and the
// bytes have to cross the plugin boundary base64-encoded, so a huge reference is refused here.
var imageMaxBytes = 24 << 20

// defaultImageMimeType is used when neither the data URL nor the response declares one.
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
)

// chatImage is one attachment as it arrived on the request: either inline base64 from a data URL
// or a reference the plugin still has to fetch.
type chatImage struct {
	URL      string
	Data     string
	MimeType string
}

// imageFetcher resolves a remote attachment to base64 bytes and a media type.
type imageFetcher func(ctx context.Context, reference string) (data string, mimeType string, err error)

// buildSdkImages converts request attachments into the wire form a local agent accepts.
//
// sdk.v1's SdkImageUrl is documented as cloud-only, so a remote reference cannot be forwarded:
// it is fetched here and sent as inline data instead. A reference the plugin cannot fetch is
// the caller's problem, not the credential's, so it fails the request with a 400.
func buildSdkImages(ctx context.Context, images []chatImage, fetch imageFetcher) ([]*sdkv1.SdkImage, error) {
	if len(images) == 0 {
		return nil, nil
	}
	converted := make([]*sdkv1.SdkImage, 0, len(images))
	for _, image := range images {
		if image.Data != "" {
			converted = append(converted, inlineSdkImage(image.Data, image.MimeType))
			continue
		}
		reference := strings.TrimSpace(image.URL)
		if reference == "" {
			continue
		}
		data, mimeType, errFetch := fetch(ctx, reference)
		if errFetch != nil {
			return nil, &upstreamError{failure: requestFault("image_unavailable", errFetch.Error())}
		}
		converted = append(converted, inlineSdkImage(data, mimeType))
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return converted, nil
}

func inlineSdkImage(data, mimeType string) *sdkv1.SdkImage {
	if strings.TrimSpace(mimeType) == "" {
		mimeType = defaultImageMimeType
	}
	return &sdkv1.SdkImage{
		Source: &sdkv1.SdkImage_Data{Data: &sdkv1.SdkImageData{Data: data, MimeType: mimeType}},
	}
}

// downloadImage fetches a remote attachment through host.http.do_stream and returns it
// base64-encoded with its media type. The URL is checked before any host call so loopback and
// private destinations never leave the plugin; the stream is closed on every path because the
// host does not recycle HTTP streams when the plugin RPC ends.
func downloadImage(ctx context.Context, reference string) (string, string, error) {
	parsed, errURL := parseFetchableImageURL(reference)
	if errURL != nil {
		return "", "", errURL
	}
	label := imageURLLabel(parsed)

	opened, errOpen := hostHTTPStream.Open(ctx, rpcHostHTTPRequest{Method: http.MethodGet, URL: parsed.String()})
	if errOpen != nil {
		return "", "", fmt.Errorf("download image %s: %w", label, errOpen)
	}
	streamID := strings.TrimSpace(opened.StreamID)
	defer func() {
		if streamID != "" {
			_ = hostHTTPStream.Close(streamID)
		}
	}()
	if opened.StatusCode < 200 || opened.StatusCode > 299 {
		return "", "", fmt.Errorf("download image %s: unexpected status %d", label, opened.StatusCode)
	}

	var body []byte
	for {
		chunk, errRead := hostHTTPStream.Read(ctx, streamID)
		if errRead != nil {
			return "", "", fmt.Errorf("download image %s: %w", label, errRead)
		}
		if chunk.Error != "" {
			return "", "", fmt.Errorf("download image %s: %s", label, chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if len(body)+len(chunk.Payload) > imageMaxBytes {
				return "", "", fmt.Errorf("download image %s exceeds the %d byte limit", label, imageMaxBytes)
			}
			body = append(body, chunk.Payload...)
		}
		if chunk.Done {
			break
		}
	}
	if len(body) == 0 {
		return "", "", fmt.Errorf("download image %s: empty response", label)
	}
	return base64.StdEncoding.EncodeToString(body), responseMimeType(opened.Headers), nil
}

// parseFetchableImageURL accepts only http(s) URLs whose host is not loopback, private,
// link-local, CGNAT or a known cloud metadata name. The error never repeats the full URL.
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
	ips, errLookup := net.LookupIP(host)
	if errLookup != nil {
		if ip := net.ParseIP(host); ip != nil {
			if blockedImageIP(ip) {
				return nil, fmt.Errorf("image host %s is not allowed", host)
			}
			return parsed, nil
		}
		return nil, fmt.Errorf("image host %s could not be resolved", host)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("image host %s could not be resolved", host)
	}
	for _, ip := range ips {
		if blockedImageIP(ip) {
			return nil, fmt.Errorf("image host %s is not allowed", host)
		}
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

// responseMimeType reads the media type from the response, dropping any charset parameter.
func responseMimeType(headers http.Header) string {
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		return ""
	}
	mimeType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return ""
	}
	return mimeType
}
