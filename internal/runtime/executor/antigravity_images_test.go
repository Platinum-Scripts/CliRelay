package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// antigravityImageUpstream is a fake CloudCode host answering streamGenerateContent
// with one image, the way the real one does for gemini-3.1-flash-image.
type antigravityImageUpstream struct {
	mu       sync.Mutex
	requests []capturedAntigravityRequest
	image    string
}

type capturedAntigravityRequest struct {
	path  string
	query string
	body  []byte
}

func (u *antigravityImageUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.requests = append(u.requests, capturedAntigravityRequest{path: r.URL.Path, query: r.URL.RawQuery, body: body})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/jpeg","data":"`+u.image+`"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":1290,"totalTokenCount":1310},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-1"},"traceId":"trace-1"}`+"\n\n")
}

func (u *antigravityImageUpstream) captured(t *testing.T) []capturedAntigravityRequest {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]capturedAntigravityRequest(nil), u.requests...)
}

func executeAntigravityImageRequest(t *testing.T, serverURL, alt, payload string) (cliproxyexecutor.Response, error) {
	t.Helper()
	openAI := sdktranslator.FromString("openai")
	return NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(serverURL), cliproxyexecutor.Request{
		Model:   "gemini-3.1-flash-image",
		Payload: []byte(payload),
		Format:  openAI,
	}, cliproxyexecutor.Options{Alt: alt, SourceFormat: openAI, OriginalRequest: []byte(payload)})
}

func TestAntigravityImagesGenerationEndToEnd(t *testing.T) {
	upstream := &antigravityImageUpstream{image: testJPEGBase64(t, 48, 27)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	resp, err := executeAntigravityImageRequest(t, server.URL, "images/generations",
		`{"model":"gemini-3.1-flash-image","prompt":"a red lighthouse","aspect_ratio":"16:9","image_size":"2k","n":1,"quality":"high"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	requests := upstream.captured(t)
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(requests))
	}
	sent := requests[0]
	// The images alt must not leak into the CloudCode URL.
	if sent.path != "/v1internal:streamGenerateContent" || sent.query != "alt=sse" {
		t.Fatalf("upstream call = %s?%s", sent.path, sent.query)
	}
	expect := map[string]string{
		"model":                           "gemini-3.1-flash-image",
		"request.contents.0.role":         "user",
		"request.contents.0.parts.0.text": "a red lighthouse",
		"request.generationConfig.imageConfig.aspectRatio":    "16:9",
		"request.generationConfig.imageConfig.imageSize":      "2K",
		"request.generationConfig.responseModalities":         "",
		"request.generationConfig.imageConfig.outputMimeType": "",
	}
	for path, want := range expect {
		if got := gjson.GetBytes(sent.body, path).String(); got != want {
			t.Errorf("upstream %s = %q, want %q (body %s)", path, got, want, sent.body)
		}
	}
	// quality has no Gemini equivalent and must not be forwarded as an unknown field.
	if strings.Contains(string(sent.body), `"quality"`) {
		t.Fatalf("quality leaked upstream: %s", sent.body)
	}

	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != upstream.image {
		t.Fatalf("b64_json does not carry the upstream image: %s", resp.Payload)
	}
	for path, want := range map[string]string{
		"data.#":              "1",
		"size":                "48x27",
		"output_format":       "jpeg",
		"usage.input_tokens":  "20",
		"usage.output_tokens": "1290",
		"usage.total_tokens":  "1310",
	} {
		if got := gjson.GetBytes(resp.Payload, path).String(); got != want {
			t.Errorf("response %s = %q, want %q", path, got, want)
		}
	}
	if gjson.GetBytes(resp.Payload, "created").Int() <= 0 {
		t.Errorf("created = %s", gjson.GetBytes(resp.Payload, "created").Raw)
	}
}

func TestAntigravityImagesEditSendsReferenceFrames(t *testing.T) {
	upstream := &antigravityImageUpstream{image: testJPEGBase64(t, 16, 16)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	reference := testPNGBase64(t)
	_, err := executeAntigravityImageRequest(t, server.URL, "images/edits",
		`{"model":"gemini-3.1-flash-image","prompt":"make it night","image":["data:image/png;base64,`+reference+`"],"size":"1024x1024"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	requests := upstream.captured(t)
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(requests))
	}
	body := requests[0].body
	if got := gjson.GetBytes(body, "request.contents.0.parts.0.text").String(); got != "make it night" {
		t.Fatalf("prompt part = %q", got)
	}
	if gjson.GetBytes(body, "request.contents.0.parts.1.inlineData.mimeType").String() != "image/png" ||
		gjson.GetBytes(body, "request.contents.0.parts.1.inlineData.data").String() != reference {
		t.Fatalf("reference part = %s", gjson.GetBytes(body, "request.contents.0.parts.1").Raw)
	}
	if gjson.GetBytes(body, "request.generationConfig.imageConfig.aspectRatio").String() != "1:1" ||
		gjson.GetBytes(body, "request.generationConfig.imageConfig.imageSize").String() != "1K" {
		t.Fatalf("size was not mapped onto imageConfig: %s", gjson.GetBytes(body, "request.generationConfig").Raw)
	}
}

// A request this layer can already tell is wrong must not spend an upstream call:
// every image call counts against a small per-account allowance.
func TestAntigravityImagesRejectsInvalidRequestsBeforeUpstream(t *testing.T) {
	upstream := &antigravityImageUpstream{image: testJPEGBase64(t, 8, 8)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	cases := map[string]string{
		"images/generations": `{"model":"gemini-3.1-flash-image","prompt":"p","aspect_ratio":"wide"}`,
		"images/edits":       `{"model":"gemini-3.1-flash-image","prompt":"p","image":"https://example.com/a.png"}`,
	}
	for alt, payload := range cases {
		_, err := executeAntigravityImageRequest(t, server.URL, alt, payload)
		var status statusErr
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.HasPrefix(status.Error(), "invalid request: ") {
			t.Fatalf("%s: error = %v, want a 400 invalid request", alt, err)
		}
	}
	if got := len(upstream.captured(t)); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func TestAntigravityImagesRejectsNonImageModel(t *testing.T) {
	upstream := &antigravityImageUpstream{image: testJPEGBase64(t, 8, 8)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	openAI := sdktranslator.FromString("openai")
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-high",
		Payload: []byte(`{"model":"gemini-3.1-pro-high","prompt":"p"}`),
		Format:  openAI,
	}, cliproxyexecutor.Options{Alt: "images/generations", SourceFormat: openAI})
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400", err)
	}
	if got := len(upstream.captured(t)); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func TestAntigravityImagesStreamIsRefused(t *testing.T) {
	openAI := sdktranslator.FromString("openai")
	_, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(context.Background(), antigravityTestAuth("http://127.0.0.1:1"), cliproxyexecutor.Request{
		Model:   "gemini-3.1-flash-image",
		Payload: []byte(`{"prompt":"p"}`),
		Format:  openAI,
	}, cliproxyexecutor.Options{Alt: "images/generations", SourceFormat: openAI})
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400", err)
	}
}

// The chat path for the same model must be untouched by the images adapter.
func TestAntigravityChatRequestForImageModelStillTranslates(t *testing.T) {
	upstream := &antigravityImageUpstream{image: testJPEGBase64(t, 8, 8)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	openAI := sdktranslator.FromString("openai")
	payload := `{"model":"gemini-3.1-flash-image","messages":[{"role":"user","content":"draw a cat"}]}`
	resp, err := NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.1-flash-image",
		Payload: []byte(payload),
		Format:  openAI,
	}, cliproxyexecutor.Options{SourceFormat: openAI, OriginalRequest: []byte(payload)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.images.0.image_url.url").String(); !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("chat response lost the image: %s", resp.Payload)
	}
}
