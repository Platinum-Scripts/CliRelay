package registry

import (
	"slices"
	"testing"
)

func TestIsGeminiImageModel(t *testing.T) {
	cases := map[string]bool{
		"gemini-3.1-flash-image":        true,
		"gemini-3-pro-image":            true,
		"gemini-2.5-flash-image":        true,
		"gemini-3-pro-image-preview":    true,
		"GEMINI-3.1-FLASH-IMAGE":        true,
		" gemini-3.1-flash-image ":      true,
		"gemini-3.1-flash":              false,
		"gemini-3.1-pro-high":           false,
		"gemini-imagery-1":              false,
		"gpt-image-2":                   false,
		"image-01":                      false,
		"imagen-4.0-generate-001":       false,
		"antigravity-image-placeholder": false,
		"":                              false,
	}
	for model, want := range cases {
		if got := IsGeminiImageModel(model); got != want {
			t.Errorf("IsGeminiImageModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// The chat request path turns image-generation models away with "use
// /v1/images instead". Gemini image models are served by the chat entrypoints
// today, so counting them as image-generation models would break those calls.
func TestGeminiImageModelsAreNotImageGenerationModels(t *testing.T) {
	for _, model := range []string{"gemini-3.1-flash-image", "gemini-2.5-flash-image", "gemini-3-pro-image-preview"} {
		if IsImageGenerationModel(model) {
			t.Fatalf("IsImageGenerationModel(%q) = true; the chat entrypoints would reject it", model)
		}
	}
}

func TestImageGenerationProviderFollowsAntigravityRegistration(t *testing.T) {
	const model = "gemini-3.1-flash-image"
	if got := ImageGenerationProvider(model); got != "" {
		t.Fatalf("provider before any credential serves the model = %q, want empty", got)
	}

	GetGlobalRegistry().RegisterClient("gemini-image-test-antigravity", "antigravity", []*ModelInfo{
		{ID: model, DisplayName: "Gemini 3.1 Flash Image"},
		{ID: "gemini-3.1-pro-high"},
	})
	t.Cleanup(func() { GetGlobalRegistry().UnregisterClient("gemini-image-test-antigravity") })

	if got := ImageGenerationProvider(model); got != ImageProviderAntigravity {
		t.Fatalf("provider = %q, want %q", got, ImageProviderAntigravity)
	}
	if got := ImageGenerationProvider("gemini-3.1-pro-high"); got != "" {
		t.Fatalf("a text model must not be offered on the images endpoint, got provider %q", got)
	}
}

// A model from the same family served only through a Gemini API key is a chat
// model there. Sending it to Antigravity would report "no credential" for a model
// that is in fact available on the chat endpoints.
func TestImageGenerationProviderIgnoresChatOnlyGeminiProviders(t *testing.T) {
	const model = "gemini-2.5-flash-image"
	GetGlobalRegistry().RegisterClient("gemini-image-test-apikey", "gemini", []*ModelInfo{{ID: model}})
	t.Cleanup(func() { GetGlobalRegistry().UnregisterClient("gemini-image-test-apikey") })

	if got := ImageGenerationProvider(model); got != "" {
		t.Fatalf("provider = %q, want empty for a model no images-capable credential serves", got)
	}
}

func TestListImageGenerationModelsIncludesAntigravityGeminiModels(t *testing.T) {
	GetGlobalRegistry().RegisterClient("gemini-image-test-list", "antigravity", []*ModelInfo{
		{ID: "gemini-3.1-flash-image", DisplayName: "Gemini 3.1 Flash Image"},
		{ID: "gemini-3.1-pro-high"},
	})
	t.Cleanup(func() { GetGlobalRegistry().UnregisterClient("gemini-image-test-list") })

	var found *ImageGenerationModel
	for _, model := range ListImageGenerationModels() {
		if model.ID == "gemini-3.1-pro-high" {
			t.Fatalf("text model listed as an image model: %+v", model)
		}
		if model.ID == "gemini-3.1-flash-image" {
			copied := model
			found = &copied
		}
	}
	if found == nil {
		t.Fatal("gemini-3.1-flash-image missing from the image model catalog")
	}
	if found.Provider != ImageProviderAntigravity || !found.SupportsEdit || found.DisplayName != "Gemini 3.1 Flash Image" {
		t.Fatalf("model = %+v", *found)
	}
	if !slices.Equal(found.AspectRatios, GeminiImageAspectRatios()) || !slices.Equal(found.ImageSizes, GeminiImageSizes()) {
		t.Fatalf("shape options = %v / %v", found.AspectRatios, found.ImageSizes)
	}
	if got := ListImageGenerationModelsForProvider(ImageProviderAntigravity); len(got) != 1 || got[0].ID != "gemini-3.1-flash-image" {
		t.Fatalf("antigravity image models = %+v", got)
	}
}

// The option lists are what upstream named in its INVALID_ARGUMENT answers; the
// console offers exactly these, so a drift here is a user-visible change.
func TestGeminiImageShapeOptionsMatchUpstream(t *testing.T) {
	ratios := GeminiImageAspectRatios()
	want := []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	sortedRatios := slices.Clone(ratios)
	slices.Sort(sortedRatios)
	sortedWant := slices.Clone(want)
	slices.Sort(sortedWant)
	if !slices.Equal(sortedRatios, sortedWant) {
		t.Fatalf("aspect ratios = %v, want the upstream set %v", ratios, want)
	}
	if ratios[0] != "1:1" {
		t.Fatalf("first aspect ratio = %q, want 1:1 as the picker default", ratios[0])
	}
	if sizes := GeminiImageSizes(); !slices.Equal(sizes, []string{"512", "1K", "2K", "4K"}) {
		t.Fatalf("image sizes = %v", sizes)
	}

	// Callers get copies, so mutating one cannot change the catalog.
	ratios[0] = "mutated"
	if GeminiImageAspectRatios()[0] != "1:1" {
		t.Fatal("GeminiImageAspectRatios returned shared storage")
	}
}

func TestSupportsImageEditingGemini(t *testing.T) {
	if !SupportsImageEditing("gemini-3.1-flash-image") {
		t.Fatal("Gemini image models take reference images inline and must offer editing")
	}
	if SupportsImageEditing("gemini-3.1-pro-high") {
		t.Fatal("a text model must not offer image editing")
	}
}
