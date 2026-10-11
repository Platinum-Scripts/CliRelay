package executor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
)

func testPNGBase64(t *testing.T) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func testJPEGBase64(t *testing.T, width, height int) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func TestParseGeminiImageRequestShape(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRatio string
		wantSize  string
	}{
		{"no shape leaves the choice to the model", `{"prompt":"p"}`, "", ""},
		{"explicit fields", `{"prompt":"p","aspect_ratio":"16:9","image_size":"2K"}`, "16:9", "2K"},
		{"lower-case tier is normalised", `{"prompt":"p","image_size":"2k"}`, "", "2K"},
		{"512 spellings collapse", `{"prompt":"p","image_size":"512px"}`, "", "512"},
		{"half-K spelling", `{"prompt":"p","image_size":"0.5K"}`, "", "512"},
		{"unknown K tier is left for upstream to judge", `{"prompt":"p","image_size":"8K"}`, "", "8K"},
		{"ratio maps onto the accepted spelling", `{"prompt":"p","aspect_ratio":"32 : 18"}`, "16:9", ""},
		// Upstream spells it 21:9; the reduced 7:3 is rejected with INVALID_ARGUMENT.
		{"21:9 is not reduced", `{"prompt":"p","aspect_ratio":"21:9"}`, "21:9", ""},
		{"7:3 maps to the 21:9 upstream accepts", `{"prompt":"p","aspect_ratio":"7:3"}`, "21:9", ""},
		{"2:2 is the square", `{"prompt":"p","aspect_ratio":"2:2"}`, "1:1", ""},
		{"an unknown ratio passes through as written", `{"prompt":"p","aspect_ratio":"34:10"}`, "34:10", ""},
		{"auto means unset", `{"prompt":"p","aspect_ratio":"auto","image_size":"auto","size":"auto"}`, "", ""},
		{"square size", `{"prompt":"p","size":"1024x1024"}`, "1:1", "1K"},
		{"landscape gpt-image size", `{"prompt":"p","size":"1536x1024"}`, "3:2", "1K"},
		{"portrait gpt-image size", `{"prompt":"p","size":"1024x1536"}`, "2:3", "1K"},
		{"dall-e wide size", `{"prompt":"p","size":"1792x1024"}`, "16:9", "1K"},
		{"2K square", `{"prompt":"p","size":"2048x2048"}`, "1:1", "2K"},
		{"4K square", `{"prompt":"p","size":"4096x4096"}`, "1:1", "4K"},
		{"512 square", `{"prompt":"p","size":"512x512"}`, "1:1", "512"},
		{"tall banner", `{"prompt":"p","size":"512x2048"}`, "1:4", "1K"},
		{"explicit ratio wins over size", `{"prompt":"p","size":"1024x1024","aspect_ratio":"4:3"}`, "4:3", "1K"},
		{"explicit tier wins over size", `{"prompt":"p","size":"1024x1024","image_size":"4K"}`, "1:1", "4K"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseGeminiImageRequest([]byte(tc.body), false)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if parsed.AspectRatio != tc.wantRatio || parsed.ImageSize != tc.wantSize {
				t.Fatalf("shape = (%q, %q), want (%q, %q)", parsed.AspectRatio, parsed.ImageSize, tc.wantRatio, tc.wantSize)
			}
			if parsed.Prompt != "p" {
				t.Fatalf("prompt = %q", parsed.Prompt)
			}
		})
	}
}

func TestParseGeminiImageRequestRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"  "}`:                                   "prompt is required",
		`{"prompt":"p","aspect_ratio":"wide"}`:              "aspect_ratio must look like 16:9",
		`{"prompt":"p","aspect_ratio":"0:9"}`:               "aspect_ratio must look like 16:9",
		`{"prompt":"p","image_size":"huge"}`:                "image_size must be one of 512, 1K, 2K, 4K",
		`{"prompt":"p","size":"big"}`:                       "size must be WIDTHxHEIGHT",
		`{"prompt":"p","response_format":"url"}`:            "only response_format=b64_json",
		`{"prompt":"p","stream":true}`:                      "streaming image generation is not supported",
		`[1,2]`:                                             "request body must be a JSON object",
		`not json`:                                          "request body must be a JSON object",
		`{"prompt":"p","response_format":"b64_json","n":2}`: "",
	}
	for body, wantErr := range cases {
		_, err := parseGeminiImageRequest([]byte(body), false)
		if wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: error = %v, want it to mention %q", body, err, wantErr)
		}
	}
}

func TestParseGeminiImageRequestReadsEveryReferenceShape(t *testing.T) {
	pngData := testPNGBase64(t)
	unpadded := strings.TrimRight(pngData, "=")
	body := `{"prompt":"make it snow",` +
		`"image_files":[{"file_name":"a.bin","content_type":"application/octet-stream","data_base64":"` + pngData + `"}],` +
		`"image":["data:image/png;base64,` + unpadded + `"],` +
		`"images":[{"image_url":"data:image/jpg;base64,` + testJPEGBase64(t, 4, 4) + `"},{"image_url":{"url":"data:image/png;base64,` + pngData + `"}}]}`

	parsed, err := parseGeminiImageRequest([]byte(body), true)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(parsed.References) != 4 {
		t.Fatalf("references = %d, want 4", len(parsed.References))
	}
	wantTypes := []string{"image/png", "image/png", "image/jpeg", "image/png"}
	for index, reference := range parsed.References {
		if reference.MimeType != wantTypes[index] {
			t.Errorf("reference %d type = %q, want %q", index, reference.MimeType, wantTypes[index])
		}
		if _, err := base64.StdEncoding.DecodeString(reference.Data); err != nil {
			t.Errorf("reference %d is not padded standard base64: %v", index, err)
		}
	}
	if parsed.References[1].Data != pngData {
		t.Fatal("unpadded base64 must be re-encoded into the padded form upstream expects")
	}
}

func TestParseGeminiImageRequestEditGuards(t *testing.T) {
	pngURI := "data:image/png;base64," + testPNGBase64(t)
	tooMany := make([]string, 0, geminiImageMaxReferenceImages+1)
	for i := 0; i <= geminiImageMaxReferenceImages; i++ {
		tooMany = append(tooMany, `"`+pngURI+`"`)
	}
	cases := map[string]string{
		`{"prompt":"p"}`: "image is required for image edits",
		`{"prompt":"p","image":"https://example.com/cat.png"}`:                                                 "remote URLs are not fetched",
		`{"prompt":"p","image":"` + pngURI + `","mask":{"image_url":"` + pngURI + `"}}`:                        "mask is not supported",
		`{"prompt":"p","image":"` + pngURI + `","mask_file":{"data_base64":"AAAA"}}`:                           "mask is not supported",
		`{"prompt":"p","image":"data:text/plain;base64,aGVsbG8gd29ybGQ="}`:                                     "must be PNG, JPEG, WEBP, HEIC or HEIF",
		`{"prompt":"p","image":"data:image/png,rawbytes"}`:                                                     "must be base64 encoded",
		`{"prompt":"p","image":"data:image/png;base64,***"}`:                                                   "not valid base64",
		`{"prompt":"p","image":[` + strings.Join(tooMany, ",") + `]}`:                                          "at most 14 reference images",
		`{"prompt":"p","image":"` + pngURI + `","mask":null}`:                                                  "",
		`{"prompt":"p","image_files":[{"content_type":"image/png","data_base64":"` + testPNGBase64(t) + `"}]}`: "",
	}
	for body, wantErr := range cases {
		label := body
		if len(label) > 80 {
			label = label[:80]
		}
		_, err := parseGeminiImageRequest([]byte(body), true)
		if wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", label, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: error = %v, want it to mention %q", label, err, wantErr)
		}
	}
}

// Generations follow the OpenAI endpoint and ignore reference images rather than
// silently turning a text-to-image call into an edit.
func TestParseGeminiImageRequestGenerationIgnoresReferences(t *testing.T) {
	parsed, err := parseGeminiImageRequest([]byte(`{"prompt":"p","image":"https://example.com/a.png"}`), false)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(parsed.References) != 0 {
		t.Fatalf("references = %d, want 0 for a generation", len(parsed.References))
	}
}

func TestGeminiImageGenerateContentPayload(t *testing.T) {
	bare, err := geminiImageRequest{Prompt: "a lighthouse"}.generateContentPayload()
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(bare, "generationConfig").Exists() {
		t.Fatalf("a request without a shape must not send generationConfig: %s", bare)
	}
	if got := gjson.GetBytes(bare, "contents.0.parts.0.text").String(); got != "a lighthouse" {
		t.Fatalf("prompt part = %q", got)
	}
	if gjson.GetBytes(bare, "generationConfig.responseModalities").Exists() {
		t.Fatal("responseModalities must stay unset: it is the request shape the chat path proved")
	}

	edit, err := geminiImageRequest{
		Prompt:      "add snow",
		AspectRatio: "16:9",
		ImageSize:   "2K",
		References:  []geminiImageReference{{MimeType: "image/png", Data: "AAAA"}, {MimeType: "image/jpeg", Data: "BBBB"}},
	}.generateContentPayload()
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(edit, "contents.0.role").String(); got != "user" {
		t.Fatalf("role = %q", got)
	}
	parts := gjson.GetBytes(edit, "contents.0.parts").Array()
	if len(parts) != 3 || parts[0].Get("text").String() != "add snow" ||
		parts[1].Get("inlineData.mimeType").String() != "image/png" || parts[1].Get("inlineData.data").String() != "AAAA" ||
		parts[2].Get("inlineData.mimeType").String() != "image/jpeg" {
		t.Fatalf("parts = %s", gjson.GetBytes(edit, "contents.0.parts").Raw)
	}
	if gjson.GetBytes(edit, "generationConfig.imageConfig.aspectRatio").String() != "16:9" ||
		gjson.GetBytes(edit, "generationConfig.imageConfig.imageSize").String() != "2K" {
		t.Fatalf("imageConfig = %s", gjson.GetBytes(edit, "generationConfig").Raw)
	}
}

func TestBuildOpenAIImagesResponseFromGemini(t *testing.T) {
	jpegData := testJPEGBase64(t, 32, 16)
	upstream := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"thought":true,"text":"planning"},` +
		`{"text":"Here is your image."},` +
		`{"inlineData":{"mimeType":"image/jpeg","data":"` + jpegData + `"}}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":37,"candidatesTokenCount":1513,"thoughtsTokenCount":5,"totalTokenCount":1555,` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":30},{"modality":"IMAGE","tokenCount":7}]},"modelVersion":"gemini-3.1-flash-image"}`

	out, err := buildOpenAIImagesResponseFromGemini([]byte(upstream), 1760000000)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	checks := map[string]string{
		"created":                                "1760000000",
		"data.#":                                 "1",
		"data.0.b64_json":                        jpegData,
		"output_format":                          "jpeg",
		"size":                                   "32x16",
		"usage.input_tokens":                     "37",
		"usage.output_tokens":                    "1518",
		"usage.total_tokens":                     "1555",
		"usage.input_tokens_details.text_tokens": "30",
		"usage.input_tokens_details.image_tokens": "7",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if gjson.GetBytes(out, "data.0.revised_prompt").Exists() {
		t.Error("the model's chatter is not a revised prompt and must not be reported as one")
	}
}

func TestBuildOpenAIImagesResponseKeepsEveryImagePart(t *testing.T) {
	png := testPNGBase64(t)
	upstream := `{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/png","data":"` + png + `"}},` +
		`{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]},"finishReason":"STOP"}]}`
	out, err := buildOpenAIImagesResponseFromGemini([]byte(upstream), 1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := gjson.GetBytes(out, "data.#").Int(); got != 2 {
		t.Fatalf("images = %d, want 2", got)
	}
	if gjson.GetBytes(out, "output_format").String() != "png" || gjson.GetBytes(out, "size").String() != "8x8" {
		t.Fatalf("format/size = %s / %s", gjson.GetBytes(out, "output_format").String(), gjson.GetBytes(out, "size").String())
	}
	if gjson.GetBytes(out, "usage").Exists() {
		t.Fatal("no usageMetadata upstream, so no usage block")
	}
}

func TestBuildOpenAIImagesResponseWithoutImage(t *testing.T) {
	cases := []struct {
		name       string
		upstream   string
		wantStatus int
		wantText   string
	}{
		{"blocked prompt", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, http.StatusBadRequest, "blocked by the upstream safety filter (PROHIBITED_CONTENT)"},
		{"image safety", `{"candidates":[{"content":{"parts":[]},"finishReason":"IMAGE_SAFETY"}]}`, http.StatusBadRequest, "no image was generated (finish reason IMAGE_SAFETY)"},
		{"refusal text with reason", `{"candidates":[{"content":{"parts":[{"text":"I can't draw that."}]},"finishReason":"IMAGE_PROHIBITED_CONTENT"}]}`, http.StatusBadRequest, "IMAGE_PROHIBITED_CONTENT): I can't draw that."},
		{"text instead of image", `{"candidates":[{"content":{"parts":[{"text":"Which style would you like?"}]},"finishReason":"STOP"}]}`, http.StatusBadRequest, "answered with text instead of an image: Which style would you like?"},
		{"empty answer", `{"candidates":[]}`, http.StatusBadGateway, "upstream returned no image"},
		{"not json", `<html>`, http.StatusBadGateway, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildOpenAIImagesResponseFromGemini([]byte(tc.upstream), 1)
			var status statusErr
			if !errors.As(err, &status) {
				t.Fatalf("error = %v, want statusErr", err)
			}
			if status.StatusCode() != tc.wantStatus || !strings.Contains(status.Error(), tc.wantText) {
				t.Fatalf("error = %d %q, want %d containing %q", status.StatusCode(), status.Error(), tc.wantStatus, tc.wantText)
			}
			// A refused prompt is the request's fault. The credential pool reads
			// "invalid request" as exactly that and leaves the account alone.
			if tc.wantStatus == http.StatusBadRequest && !strings.HasPrefix(status.Error(), "invalid request: ") {
				t.Fatalf("message %q must carry the invalid-request signal", status.Error())
			}
		})
	}
}

// Every value the console offers must reach upstream exactly as upstream spells
// it. Reducing 21:9 to 7:3 is how a picker choice once came back as a 400.
func TestNormalizeGeminiAspectRatioKeepsEveryAcceptedRatio(t *testing.T) {
	for _, ratio := range registry.GeminiImageAspectRatios() {
		got, err := normalizeGeminiAspectRatio(ratio)
		if err != nil || got != ratio {
			t.Errorf("normalizeGeminiAspectRatio(%q) = %q, %v; want it unchanged", ratio, got, err)
		}
	}
	for _, bad := range []string{"0:1", "1:0", "16x9", "99999:1", "-1:2", "wide"} {
		if _, err := normalizeGeminiAspectRatio(bad); err == nil {
			t.Errorf("normalizeGeminiAspectRatio(%q) accepted a malformed ratio", bad)
		}
	}
}

func TestNearestGeminiAspectRatioCoversEveryAcceptedRatio(t *testing.T) {
	for _, ratio := range []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9", "1:4", "4:1", "1:8", "8:1"} {
		w, h, ok := parseImageSizeDimensions(strings.Replace(ratio, ":", "x", 1))
		if !ok {
			t.Fatalf("cannot parse %s", ratio)
		}
		if got := nearestGeminiAspectRatio(w*100, h*100); got != ratio {
			t.Errorf("nearest ratio for %dx%d = %s, want %s", w*100, h*100, got, ratio)
		}
	}
}
