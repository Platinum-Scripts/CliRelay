package registry

import (
	"sort"
	"strings"
)

// Gemini image models.
//
// Gemini's image models (gemini-3.1-flash-image and its relatives) are chat models
// whose responses carry the generated picture as inline data. Two things set them
// apart from the image-only families in image_generation_models.go, and are why
// IsImageGenerationModel deliberately leaves them out:
//
//   - Every chat entrypoint already serves them. The translators carry the inline
//     image back as message.images, a Claude image block or a Responses
//     image_generation_call. IsImageGenerationModel is what the request path uses
//     to turn a model away with "use /v1/images instead", so counting them there
//     would break four entrypoints that work today.
//   - Upstream meters them in tokens. The per-call billing seed that
//     IsImageGenerationModel drives would rewrite their pricing.
//
// They are reachable through /v1/images/* as well: the Antigravity executor adapts
// that request shape onto generateContent. ImageGenerationProvider is the gate for
// those endpoints, so the models show up there and in the console catalog — but
// only while an Antigravity credential actually serves them.

// IsGeminiImageModel reports whether a model id names a Gemini image model.
//
// The rule is the family's naming, a "gemini-" id with an "image" segment
// (gemini-2.5-flash-image, gemini-3-pro-image-preview, gemini-3.1-flash-image),
// rather than a list: Antigravity publishes its image models dynamically, and a new
// release must not need a code change before it can be classified.
func IsGeminiImageModel(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if !strings.HasPrefix(normalized, "gemini-") {
		return false
	}
	for _, segment := range strings.Split(normalized, "-") {
		if segment == "image" {
			return true
		}
	}
	return false
}

// geminiImageAspectRatios are the aspect ratios the upstream accepts, in the order
// a picker should offer them: the everyday shapes first, the banner extremes last.
//
// The set is the upstream's own. Antigravity answers anything else with
// INVALID_ARGUMENT naming exactly these fourteen (measured against
// gemini-3.1-flash-image on 2026-10-11).
var geminiImageAspectRatios = []string{
	"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9",
	"4:1", "1:4", "8:1", "1:8",
}

// geminiImageSizes are the output size tiers, smallest first.
//
// An unknown tier is answered with "Supported values are: 1K, 2K, 4K, 512, 512P,
// 512PX". 512P and 512PX are spellings of 512, so they are not offered separately.
// The tier sets the pixel budget rather than the exact dimensions: 1:4 at 1K came
// back as 512x2064, and the default shape at 2K as 2816x1536.
var geminiImageSizes = []string{"512", "1K", "2K", "4K"}

// GeminiImageAspectRatios returns the aspect ratios a Gemini image model accepts.
func GeminiImageAspectRatios() []string {
	return append([]string(nil), geminiImageAspectRatios...)
}

// GeminiImageSizes returns the output size tiers a Gemini image model accepts.
func GeminiImageSizes() []string {
	return append([]string(nil), geminiImageSizes...)
}

// geminiImageGenerationProvider names the credential pool that serves a Gemini
// image model on /v1/images/*, or "" when none does.
//
// Only the Antigravity executor adapts the images request shape. The answer comes
// from the live registry rather than from the id, because the Gemini API-key and
// Vertex providers serve ids from the same family through chat only: sending one of
// their models to Antigravity would fail as "no credential" for a model that is in
// fact available, just not on this endpoint.
func geminiImageGenerationProvider(modelID string) string {
	for _, provider := range GetGlobalRegistry().GetModelProviders(strings.TrimSpace(modelID)) {
		if strings.EqualFold(provider, ImageProviderAntigravity) {
			return ImageProviderAntigravity
		}
	}
	return ""
}

// listGeminiImageGenerationModels returns the Gemini image models an Antigravity
// credential currently serves, sorted by id so the console renders them in a
// stable order.
func listGeminiImageGenerationModels() []ImageGenerationModel {
	infos := GetGlobalRegistry().GetAvailableModelsByProvider(ImageProviderAntigravity)
	models := make([]ImageGenerationModel, 0, 1)
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info == nil || !IsGeminiImageModel(info.ID) {
			continue
		}
		if _, ok := seen[info.ID]; ok {
			continue
		}
		seen[info.ID] = struct{}{}
		models = append(models, ImageGenerationModel{
			ID:           info.ID,
			Provider:     ImageProviderAntigravity,
			DisplayName:  info.DisplayName,
			Description:  info.Description,
			SupportsEdit: true,
			AspectRatios: GeminiImageAspectRatios(),
			ImageSizes:   GeminiImageSizes(),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}
