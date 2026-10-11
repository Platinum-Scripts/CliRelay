package handlers

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Gemini image models gained a provider on /v1/images/*. The chat entrypoints
// must keep serving them: four of them already return the generated picture, and
// the guard that sends image-only models to /v1/images must not catch these.
func TestGetRequestDetails_ServesGeminiImageModelOnChat(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("test-request-details-antigravity-image", "antigravity", []*registry.ModelInfo{
		{ID: "gemini-3.1-flash-image"},
	})
	modelRegistry.RegisterClient("test-request-details-codex-image", "codex", []*registry.ModelInfo{
		{ID: "gpt-image-2"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("test-request-details-antigravity-image")
		modelRegistry.UnregisterClient("test-request-details-codex-image")
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	providers, model, errMsg := handler.getRequestDetails(context.Background(), "gemini-3.1-flash-image")
	if errMsg != nil {
		t.Fatalf("getRequestDetails() error = %v", errMsg.Error)
	}
	if !reflect.DeepEqual(providers, []string{"antigravity"}) || model != "gemini-3.1-flash-image" {
		t.Fatalf("providers/model = %v/%q", providers, model)
	}

	_, _, errMsg = handler.getRequestDetails(context.Background(), "gpt-image-2")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("gpt-image-2 on the chat path = %+v, want the 400 that points to /v1/images", errMsg)
	}
}
