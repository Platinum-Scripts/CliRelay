package openai

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func newGeminiImageTestRouter(t *testing.T, provider string, models ...string) (*gin.Engine, *imageProviderCaptureExecutor) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &imageProviderCaptureExecutor{identifier: provider}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestProviderAuth(t, manager, provider+"-gemini-image-auth", provider, "Team "+provider, models...)

	h := NewOpenAIImagesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)
	router.POST("/v1/images/edits", h.Edits)
	return router, executor
}

func TestOpenAIImagesGenerationsRoutesGeminiImageModelToAntigravity(t *testing.T) {
	router, executor := newGeminiImageTestRouter(t, "antigravity", "gemini-3.1-flash-image")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gemini-3.1-flash-image","prompt":"a lighthouse","aspect_ratio":"21:9","image_size":"4K"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if executor.calls != 1 || executor.alt != "images/generations" || executor.model != "gemini-3.1-flash-image" {
		t.Fatalf("calls=%d alt=%q model=%q", executor.calls, executor.alt, executor.model)
	}
	if gjson.Get(executor.payload, "aspect_ratio").String() != "21:9" || gjson.Get(executor.payload, "image_size").String() != "4K" {
		t.Fatalf("payload = %s", executor.payload)
	}
}

// A Gemini image model served only through a chat-only provider is not offered
// here; the caller is told plainly instead of reaching a credential that cannot
// answer the images request.
func TestOpenAIImagesGenerationsRejectsGeminiImageModelWithoutAntigravity(t *testing.T) {
	router, executor := newGeminiImageTestRouter(t, "gemini", "gemini-2.5-flash-image")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"a lighthouse"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "not a supported image generation model") {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
}

func TestOpenAIImagesMultipartEditKeepsGeminiShape(t *testing.T) {
	router, executor := newGeminiImageTestRouter(t, "antigravity", "gemini-3.1-flash-image")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{
		"model":        "gemini-3.1-flash-image",
		"prompt":       "make it night",
		"aspect_ratio": "9:16",
		"image_size":   "2K",
	} {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image", "ref.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if executor.alt != "images/edits" {
		t.Fatalf("alt = %q", executor.alt)
	}
	if gjson.Get(executor.payload, "aspect_ratio").String() != "9:16" || gjson.Get(executor.payload, "image_size").String() != "2K" ||
		gjson.Get(executor.payload, "image_files.#").Int() != 1 {
		t.Fatalf("payload = %s", executor.payload)
	}
}
