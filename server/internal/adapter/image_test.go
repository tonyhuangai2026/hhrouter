package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// pngB64 is a tiny but REAL 1x1 PNG (valid magic bytes 89 50 4E 47…) reused
// across image tests. It must be a genuine PNG because the Bedrock path now
// sniffs the decoded bytes to determine the Converse format.
const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/IjBAAAAAElFTkSuQmCC"

// ============================ Inbound → unified ============================

// TestOpenAIInbound_ImageParts covers the OpenAI content-array → unified image
// block parsing for both a base64 data URL and a plain http URL, mixed with text.
func TestOpenAIInbound_ImageParts(t *testing.T) {
	dataURL := "data:image/png;base64," + pngB64
	tests := []struct {
		name      string
		content   string // raw JSON for the message content field
		wantTexts []string
		wantImage *ImageSource
	}{
		{
			name:      "plain string stays one text block",
			content:   `"just text"`,
			wantTexts: []string{"just text"},
		},
		{
			name:      "text part only",
			content:   `[{"type":"text","text":"hi"}]`,
			wantTexts: []string{"hi"},
		},
		{
			name:      "base64 data url image",
			content:   `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]`,
			wantTexts: []string{"look"},
			wantImage: &ImageSource{Kind: ImageKindBase64, MediaType: "image/png", Data: pngB64},
		},
		{
			name:      "http url image",
			content:   `[{"type":"image_url","image_url":{"url":"https://ex.com/a.jpg"}}]`,
			wantImage: &ImageSource{Kind: ImageKindURL, URL: "https://ex.com/a.jpg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := OpenAIChatInbound{
				Model:    "gpt-4o",
				Messages: []OpenAIInboundMessage{{Role: RoleUser, Content: json.RawMessage(tc.content)}},
			}
			uni := ParseOpenAIRequest(in)
			if len(uni.Messages) != 1 {
				t.Fatalf("messages = %d, want 1", len(uni.Messages))
			}
			assertBlocks(t, uni.Messages[0].Content, tc.wantTexts, tc.wantImage)
		})
	}
}

// TestAnthropicInbound_ImageBlocks covers Anthropic image source parsing
// (base64 and url) → unified.
func TestAnthropicInbound_ImageBlocks(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantTexts []string
		wantImage *ImageSource
	}{
		{
			name:      "base64 source",
			content:   `[{"type":"text","text":"q"},{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"` + pngB64 + `"}}]`,
			wantTexts: []string{"q"},
			wantImage: &ImageSource{Kind: ImageKindBase64, MediaType: "image/webp", Data: pngB64},
		},
		{
			name:      "url source",
			content:   `[{"type":"image","source":{"type":"url","url":"https://ex.com/p.gif"}}]`,
			wantImage: &ImageSource{Kind: ImageKindURL, URL: "https://ex.com/p.gif"},
		},
		{
			name:      "plain string",
			content:   `"hello"`,
			wantTexts: []string{"hello"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := AnthropicInbound{
				Model:    "claude",
				Messages: []AnthropicMessage{{Role: RoleUser, Content: json.RawMessage(tc.content)}},
			}
			uni := ParseAnthropicRequest(in)
			if len(uni.Messages) != 1 {
				t.Fatalf("messages = %d, want 1", len(uni.Messages))
			}
			assertBlocks(t, uni.Messages[0].Content, tc.wantTexts, tc.wantImage)
		})
	}
}

// ============================ unified → outbound ===========================

// TestOpenAIOutbound_ImageContent verifies a unified image message renders to an
// OpenAI image_url parts array (base64 → data: URL, url → passthrough), while a
// text-only message renders to a plain string (legacy byte-identical shape).
func TestOpenAIOutbound_ImageContent(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelOpenAI, BaseURL: "https://api.example.com"}

	uni := UnifiedRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{
				TextBlock("describe"),
				ImageBase64Block("image/png", pngB64),
				ImageURLBlock("https://ex.com/x.jpg"),
			}},
		},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	// Decode the raw body generically since content is now polymorphic.
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	var parts []openAIPart
	if err := json.Unmarshal(body.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content should be a parts array: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe" {
		t.Errorf("part0 = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil ||
		parts[1].ImageURL.URL != "data:image/png;base64,"+pngB64 {
		t.Errorf("part1 (base64) = %+v", parts[1])
	}
	if parts[2].Type != "image_url" || parts[2].ImageURL == nil ||
		parts[2].ImageURL.URL != "https://ex.com/x.jpg" {
		t.Errorf("part2 (url) = %+v", parts[2])
	}
}

// TestOpenAIOutbound_TextOnlyStaysString is the no-regression guard: a text-only
// message must serialize its content as a plain JSON string, not an array.
func TestOpenAIOutbound_TextOnlyStaysString(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelOpenAI, BaseURL: "https://api.example.com"}
	uni := UnifiedRequest{
		Model:    "gpt-4o",
		System:   "be brief",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}}},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system+user)", len(body.Messages))
	}
	for i, m := range body.Messages {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			t.Errorf("message %d content should be a string, got %s", i, m.Content)
		}
	}
}

// TestAnthropicOutbound_ImageBlocks verifies unified image blocks render to the
// Anthropic {type:image,source:{...}} shape for both kinds, plus text.
func TestAnthropicOutbound_ImageBlocks(t *testing.T) {
	content := []ContentBlock{
		TextBlock("q"),
		ImageBase64Block("image/jpeg", pngB64),
		ImageURLBlock("https://ex.com/y.png"),
	}
	blocks := BuildAnthropicContentBlocks(content)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "q" {
		t.Errorf("block0 = %+v", blocks[0])
	}
	src1 := blocks[1]["source"].(map[string]any)
	if blocks[1]["type"] != "image" || src1["type"] != "base64" ||
		src1["media_type"] != "image/jpeg" || src1["data"] != pngB64 {
		t.Errorf("block1 = %+v", blocks[1])
	}
	src2 := blocks[2]["source"].(map[string]any)
	if blocks[2]["type"] != "image" || src2["type"] != "url" || src2["url"] != "https://ex.com/y.png" {
		t.Errorf("block2 = %+v", blocks[2])
	}
}

// TestBedrockOutbound_Base64Image verifies a unified base64 image becomes a
// Converse image{format,source.bytes} block (no network needed), and that
// text-only messages still serialize to text blocks (no regression).
func TestBedrockOutbound_Base64Image(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model: "anthropic.claude",
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{
				TextBlock("look"),
				// Declared image/jpeg but the bytes are a real PNG: sniffing the
				// magic bytes is the source of truth, so the Converse format must be
				// "png" (NOT the mislabeled "jpeg") — exactly the mismatch that
				// previously produced Bedrock's "Could not process image".
				ImageBase64Block("image/jpeg", pngB64),
			}},
		},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body bedrockConverseRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	blocks := body.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Text != "look" || blocks[0].Image != nil {
		t.Errorf("block0 = %+v", blocks[0])
	}
	if blocks[1].Image == nil || blocks[1].Image.Format != "png" || blocks[1].Image.Source.Bytes != pngB64 {
		t.Errorf("block1 = %+v (want sniffed format png)", blocks[1].Image)
	}
}

// TestBedrockOutbound_URLImageDownloaded verifies a url image is downloaded and
// inlined as base64 for Bedrock, with the format SNIFFED from the actual bytes
// (the Content-Type is advisory only — sniffing is the source of truth).
func TestBedrockOutbound_URLImageDownloaded(t *testing.T) {
	// Real WebP magic: "RIFF"<4 size bytes>"WEBP". The server also reports
	// image/webp; both agree, so the Converse format is webp.
	rawBytes := []byte("RIFF\x00\x00\x00\x00WEBPfake-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(rawBytes)
	}))
	defer srv.Close()

	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model:    "anthropic.claude",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{ImageURLBlock(srv.URL + "/img")}}},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body bedrockConverseRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img := body.Messages[0].Content[0].Image
	if img == nil {
		t.Fatal("expected image block")
	}
	if img.Format != "webp" {
		t.Errorf("format = %q, want webp (sniffed from RIFF/WEBP magic)", img.Format)
	}
	if want := base64.StdEncoding.EncodeToString(rawBytes); img.Source.Bytes != want {
		t.Errorf("bytes = %q, want %q", img.Source.Bytes, want)
	}
}

// TestBedrockOutbound_URLImageDownloadError verifies a failed download aborts
// BuildRequest with a readable error rather than silently dropping the image.
func TestBedrockOutbound_URLImageDownloadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model:    "anthropic.claude",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{ImageURLBlock(srv.URL + "/img")}}},
	}
	_, err := a.BuildRequest(context.Background(), uni, ch)
	if err == nil {
		t.Fatal("expected error on failed image download")
	}
	if !strings.Contains(err.Error(), "image download") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want a readable download/403 error", err)
	}
}

// TestSniffImageFormat covers magic-byte detection for the four Bedrock-supported
// formats plus the unsupported/garbage cases that must yield "".
func TestSniffImageFormat(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(pngB64)
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"png", png, "png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, "jpeg"},
		{"gif87", []byte("GIF87a______"), "gif"},
		{"gif89", []byte("GIF89a______"), "gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP____"), "webp"},
		{"bmp-unsupported", []byte("BM\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), ""},
		{"heic-unsupported", []byte("\x00\x00\x00\x18ftypheic"), ""},
		{"too-short", []byte{0x89, 0x50}, ""},
	}
	for _, tc := range cases {
		if got := sniffImageFormat(tc.raw); got != tc.want {
			t.Errorf("sniffImageFormat(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBedrockOutbound_MislabeledImageSniffed proves a base64 image whose declared
// media type is wrong (or unsupported) is sent to Bedrock with the SNIFFED
// format, not the bogus label — the direct fix for the "Could not process image"
// failure when a client (e.g. the Playground) reports the wrong MIME type.
func TestBedrockOutbound_MislabeledImageSniffed(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	// Declared image/heic (which Bedrock can't take), but the bytes are a real PNG.
	uni := UnifiedRequest{
		Model:    "anthropic.claude",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{ImageBase64Block("image/heic", pngB64)}}},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body bedrockConverseRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img := body.Messages[0].Content[0].Image
	if img == nil || img.Format != "png" {
		t.Errorf("format = %+v, want png (sniffed despite image/heic label)", img)
	}
}

// TestBedrockOutbound_UnsupportedImageRejected proves an image that is genuinely
// not one of Bedrock's four formats (and not recoverable by the declared label)
// fails BuildRequest with a clear, actionable error instead of being mislabeled
// as png and bouncing off the upstream with the opaque "Could not process image".
func TestBedrockOutbound_UnsupportedImageRejected(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	// BMP magic ("BM…"), declared image/bmp — neither sniff nor label is supported.
	bmp := base64.StdEncoding.EncodeToString([]byte("BM\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00ZZ"))
	uni := UnifiedRequest{
		Model:    "anthropic.claude",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{ImageBase64Block("image/bmp", bmp)}}},
	}
	_, err := a.BuildRequest(context.Background(), uni, ch)
	if err == nil {
		t.Fatal("expected an unsupported-format error")
	}
	if !strings.Contains(err.Error(), "unsupported image format") || !strings.Contains(err.Error(), "PNG, JPEG, GIF, or WebP") {
		t.Errorf("error = %v, want a readable unsupported-format message", err)
	}
}

// TestBedrockOutbound_URLImageOverCap verifies the size cap is enforced.
func TestBedrockOutbound_URLImageOverCap(t *testing.T) {
	big := make([]byte, MaxImageDownloadBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	_, _, err := DownloadImageToBase64(context.Background(), srv.URL+"/big.png")
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected size-cap error, got %v", err)
	}
}

// ============================ Round-trip across formats ====================

// TestImageRoundTrip_OpenAIToUnifiedToAllOutbound takes an OpenAI inbound image
// (data: URL) through the unified layer and out to all three outbound formats,
// asserting the bytes survive end-to-end.
func TestImageRoundTrip_OpenAIToUnifiedToAllOutbound(t *testing.T) {
	dataURL := "data:image/png;base64," + pngB64
	in := OpenAIChatInbound{
		Model: "gpt-4o",
		Messages: []OpenAIInboundMessage{
			{Role: RoleUser, Content: json.RawMessage(
				`[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]`)},
		},
	}
	uni := ParseOpenAIRequest(in)

	// → OpenAI
	oaContent := openAIOutboundContent(uni.Messages[0])
	parts, ok := oaContent.([]openAIOutboundPart)
	if !ok || len(parts) != 2 || parts[1].ImageURL.URL != dataURL {
		t.Errorf("openai outbound = %+v", oaContent)
	}

	// → Anthropic
	ab := BuildAnthropicContentBlocks(uni.Messages[0].Content)
	src := ab[1]["source"].(map[string]any)
	if src["type"] != "base64" || src["data"] != pngB64 || src["media_type"] != "image/png" {
		t.Errorf("anthropic outbound = %+v", ab[1])
	}

	// → Bedrock (base64, no network)
	bd, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	img := bd.Messages[0].Content[1].Image
	if img == nil || img.Format != "png" || img.Source.Bytes != pngB64 {
		t.Errorf("bedrock outbound = %+v", img)
	}
}

// TestImageRoundTrip_AnthropicToOpenAI takes an Anthropic inbound base64 image
// through unified and out to the OpenAI data: URL form.
func TestImageRoundTrip_AnthropicToOpenAI(t *testing.T) {
	in := AnthropicInbound{
		Model: "claude",
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(
				`[{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"` + pngB64 + `"}}]`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	content := openAIOutboundContent(uni.Messages[0])
	parts := content.([]openAIOutboundPart)
	if parts[0].ImageURL.URL != "data:image/gif;base64,"+pngB64 {
		t.Errorf("openai data url = %q", parts[0].ImageURL.URL)
	}
}

// ============================ helper-unit coverage =========================

func TestParseDataURL(t *testing.T) {
	tests := []struct {
		url      string
		wantMT   string
		wantData string
		wantOK   bool
	}{
		{"data:image/png;base64,AAAA", "image/png", "AAAA", true},
		{"data:;base64,BBBB", "", "BBBB", true},
		{"https://ex.com/a.png", "", "", false},
		{"data:image/png,notb64", "", "", false},
		{"data:image/jpeg;base64,", "image/jpeg", "", true},
	}
	for _, tc := range tests {
		mt, data, ok := parseDataURL(tc.url)
		if ok != tc.wantOK || mt != tc.wantMT || data != tc.wantData {
			t.Errorf("parseDataURL(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.url, mt, data, ok, tc.wantMT, tc.wantData, tc.wantOK)
		}
	}
}

func TestBedrockImageFormat(t *testing.T) {
	cases := map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/jpg": "jpeg",
		"jpg": "jpeg", "image/gif": "gif", "image/webp": "webp",
		// Unsupported/empty types now yield "" (no silent png default) so the
		// caller can sniff the bytes or reject with a readable error.
		"image/svg+xml": "", "image/heic": "", "": "",
	}
	for in, want := range cases {
		if got := bedrockImageFormat(in); got != want {
			t.Errorf("bedrockImageFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMediaTypeFromURL(t *testing.T) {
	cases := map[string]string{
		"https://e.com/a.PNG":      "image/png",
		"https://e.com/a.jpeg?x=1": "image/jpeg",
		"https://e.com/a.gif#frag": "image/gif",
		"https://e.com/a.webp":     "image/webp",
		"https://e.com/noext":      "image/png",
		"https://e.com/a.bmp":      "image/png",
	}
	for in, want := range cases {
		if got := mediaTypeFromURL(in); got != want {
			t.Errorf("mediaTypeFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// assertBlocks asserts a unified content slice has the expected text blocks (in
// order) plus an optional single image block matching wantImage.
func assertBlocks(t *testing.T, blocks []ContentBlock, wantTexts []string, wantImage *ImageSource) {
	t.Helper()
	var gotTexts []string
	var gotImage *ImageSource
	for _, b := range blocks {
		switch {
		case b.Type == BlockText:
			gotTexts = append(gotTexts, b.Text)
		case b.IsImage():
			gotImage = b.Image
		}
	}
	if strings.Join(gotTexts, "|") != strings.Join(wantTexts, "|") {
		t.Errorf("text blocks = %v, want %v", gotTexts, wantTexts)
	}
	if wantImage == nil {
		if gotImage != nil {
			t.Errorf("unexpected image block: %+v", gotImage)
		}
		return
	}
	if gotImage == nil {
		t.Fatalf("expected image block %+v, got none", wantImage)
	}
	if *gotImage != *wantImage {
		t.Errorf("image = %+v, want %+v", *gotImage, *wantImage)
	}
}
