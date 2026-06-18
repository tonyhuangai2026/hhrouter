package adapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// image.go holds the image-multimodal helpers shared across the inbound
// transforms and the three outbound builders (OpenAI / Anthropic / Bedrock):
//   - parsing/encoding of `data:<media-type>;base64,<data>` URLs,
//   - mapping an IANA media type to a Bedrock Converse image `format`,
//   - downloading a remote image URL to inline base64 (with a size cap and
//     timeout) for the Bedrock path, which only accepts inline bytes.

// MaxImageDownloadBytes caps the size of an image fetched from a URL when an
// upstream (Bedrock Converse) requires inline bytes. ~5 MB per the Tech Design.
const MaxImageDownloadBytes = 5 << 20

// imageDownloadTimeout bounds a single URL→bytes fetch.
const imageDownloadTimeout = 15 * time.Second

// parseDataURL splits a `data:<media-type>;base64,<base64>` URL into its media
// type and base64 payload (payload returned WITHOUT the prefix). ok is false for
// any URL that is not a base64 data URL (e.g. a plain http(s) URL or a
// non-base64 data URL), in which case the caller should treat it as a URL ref.
//
// A data URL with an empty/omitted media type (e.g. "data:;base64,...") yields
// ok=true with an empty mediaType.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := url[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	// meta is "<media-type>;base64" or just ";base64" or "<media-type>" (not b64).
	if !strings.Contains(meta, "base64") {
		return "", "", false
	}
	mediaType = meta
	if i := strings.IndexByte(meta, ';'); i >= 0 {
		mediaType = meta[:i]
	}
	return strings.TrimSpace(mediaType), payload, true
}

// buildDataURL assembles a `data:<media-type>;base64,<data>` URL. A missing
// media type defaults to image/png so the result is always a well-formed data
// URL that OpenAI-compatible upstreams accept.
func buildDataURL(mediaType, data string) string {
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + data
}

// bedrockImageFormat maps an IANA image media type to the Converse image format
// token. Bedrock accepts png | jpeg | gif | webp. It tolerates both
// "image/jpeg" and a bare "jpeg", and normalizes "jpg" → "jpeg". An
// unknown/empty/unsupported media type yields "" (the caller should then sniff
// the bytes or reject), NOT a silent "png" default — mislabeling the format is
// exactly what makes Bedrock reply with the opaque "Could not process image".
func bedrockImageFormat(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, '/'); i >= 0 {
		mt = mt[i+1:]
	}
	switch mt {
	case "png":
		return "png"
	case "jpeg", "jpg":
		return "jpeg"
	case "gif":
		return "gif"
	case "webp":
		return "webp"
	default:
		return ""
	}
}

// sniffImageFormat inspects the leading magic bytes of a decoded image and
// returns the Bedrock Converse format token (png | jpeg | gif | webp), or "" if
// the bytes are not one of the four formats Bedrock supports. This is the source
// of truth for the format: the declared media type (from a browser File.type, a
// data: URL, or a URL extension) is frequently wrong or unsupported (HEIC, AVIF,
// BMP, SVG, or a plain mislabel), and Converse validates the actual bytes
// against the declared format — a mismatch is the "Could not process image"
// failure. Sniffing avoids both the mislabel and the silent png default.
func sniffImageFormat(raw []byte) string {
	if len(raw) < 12 {
		return ""
	}
	switch {
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	case raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4E && raw[3] == 0x47:
		return "png"
	// JPEG: FF D8 FF
	case raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "jpeg"
	// GIF: "GIF87a" / "GIF89a"
	case raw[0] == 'G' && raw[1] == 'I' && raw[2] == 'F' && raw[3] == '8':
		return "gif"
	// WEBP: "RIFF"...."WEBP"
	case raw[0] == 'R' && raw[1] == 'I' && raw[2] == 'F' && raw[3] == 'F' &&
		raw[8] == 'W' && raw[9] == 'E' && raw[10] == 'B' && raw[11] == 'P':
		return "webp"
	default:
		return ""
	}
}

// imageFormatLabel renders a human-readable description of an image's declared
// media type for error messages, falling back to "unknown" when absent.
func imageFormatLabel(mediaType string) string {
	mt := strings.TrimSpace(mediaType)
	if mt == "" {
		return "unknown"
	}
	return mt
}

// downloadHTTPClient is the client used to fetch remote images for the Bedrock
// (inline-bytes-only) path. Package-level so tests can override transport.
var downloadHTTPClient = &http.Client{Timeout: imageDownloadTimeout}

// DownloadImageToBase64 fetches a remote image URL and returns its bytes as
// base64 plus the media type reported by the server (falling back to inferring
// from the URL extension). It enforces MaxImageDownloadBytes and a timeout, and
// returns a readable error on any failure so the relay/controller can surface it.
//
// This exists for the Bedrock Converse path, which only accepts inline image
// bytes — a unified url-image must be materialized before it can be sent. Only
// http(s) URLs are downloaded; a base64 data URL is decoded locally without a
// network call.
func DownloadImageToBase64(ctx context.Context, url string) (mediaType, data string, err error) {
	// A base64 data URL needs no network round-trip.
	if mt, b64, ok := parseDataURL(url); ok {
		return mt, b64, nil
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", "", fmt.Errorf("image download: unsupported URL scheme: %q", snippetURL(url))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("image download: build request: %w", err)
	}
	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("image download: fetch %q: %w", snippetURL(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("image download: %q returned status %d", snippetURL(url), resp.StatusCode)
	}

	// Read at most MaxImageDownloadBytes+1 so we can detect an over-cap body.
	limited := io.LimitReader(resp.Body, MaxImageDownloadBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return "", "", fmt.Errorf("image download: read body of %q: %w", snippetURL(url), err)
	}
	if len(raw) > MaxImageDownloadBytes {
		return "", "", fmt.Errorf("image download: %q exceeds %d-byte cap", snippetURL(url), MaxImageDownloadBytes)
	}

	mediaType = resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mediaType, ';'); i >= 0 { // strip "; charset=..."
		mediaType = mediaType[:i]
	}
	mediaType = strings.TrimSpace(mediaType)
	if mediaType == "" || !strings.HasPrefix(mediaType, "image/") {
		mediaType = mediaTypeFromURL(url)
	}
	return mediaType, base64.StdEncoding.EncodeToString(raw), nil
}

// mediaTypeFromURL guesses an image media type from a URL's file extension,
// defaulting to image/png.
func mediaTypeFromURL(url string) string {
	u := url
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	dot := strings.LastIndexByte(u, '.')
	if dot < 0 {
		return "image/png"
	}
	switch strings.ToLower(u[dot+1:]) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// snippetURL trims a URL for inclusion in an error message so a very long data
// URL or signed URL doesn't bloat logs.
func snippetURL(url string) string {
	const max = 120
	if len(url) > max {
		return url[:max] + "…"
	}
	return url
}
