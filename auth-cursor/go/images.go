package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// imageMaxBytes caps a downloaded attachment. Cursor rejects oversized images anyway, and the
// bytes have to cross the plugin boundary base64-encoded, so a huge reference is refused here.
const imageMaxBytes = 24 << 20

// defaultImageMimeType is used when neither the data URL nor the response declares one.
const defaultImageMimeType = "image/png"

// chatImage is one attachment as it arrived on the request: either inline base64 from a data URL
// or a reference the plugin still has to fetch.
type chatImage struct {
	URL      string
	Data     string
	MimeType string
}

// imageFetcher resolves a remote attachment to base64 bytes and a media type.
type imageFetcher func(reference string) (data string, mimeType string, err error)

// buildSdkImages converts request attachments into the wire form a local agent accepts.
//
// sdk.v1's SdkImageUrl is documented as cloud-only, so a remote reference cannot be forwarded:
// it is fetched here and sent as inline data instead. A reference the plugin cannot fetch is
// the caller's problem, not the credential's, so it fails the request with a 400.
func buildSdkImages(images []chatImage, fetch imageFetcher) ([]*sdkv1.SdkImage, error) {
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
		data, mimeType, errFetch := fetch(reference)
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

// downloadImage fetches a remote attachment and returns it base64-encoded with its media type.
func downloadImage(reference string) (string, string, error) {
	resp, errDo := hostHTTPDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: reference})
	if errDo != nil {
		return "", "", fmt.Errorf("download image %s: %w", reference, errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("download image %s: unexpected status %d", reference, resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		return "", "", fmt.Errorf("download image %s: empty response", reference)
	}
	if len(resp.Body) > imageMaxBytes {
		return "", "", fmt.Errorf("download image %s: %d bytes exceeds the %d byte limit", reference, len(resp.Body), imageMaxBytes)
	}
	return base64.StdEncoding.EncodeToString(resp.Body), responseMimeType(resp.Headers), nil
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
