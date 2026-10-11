package executor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // registers the decoder image.DecodeConfig uses for the size report
	_ "image/png"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
)

// The OpenAI images API on top of Gemini image models.
//
// A /v1/images/* request names a prompt, an output shape and, for edits, reference
// images. A Gemini image model takes the same intent as an ordinary generateContent
// call: the prompt and the reference frames are parts of one user turn, and the
// shape is set by generationConfig.imageConfig. The helpers here translate between
// the two so an executor that already speaks generateContent can serve the images
// endpoints without a second request path.
//
// The mapping follows what Antigravity's gemini-3.1-flash-image accepted when
// measured on 2026-10-11:
//   - aspect_ratio and image_size go to imageConfig.aspectRatio and
//     imageConfig.imageSize. Upstream validates both and names the accepted values
//     in its INVALID_ARGUMENT answer, so they are normalised here but not checked
//     against a list — a tier or ratio Google adds later works without a release.
//   - A WIDTHxHEIGHT size has no upstream equivalent. It becomes the nearest
//     accepted ratio and the nearest size tier by pixel count, so a client written
//     for gpt-image still gets roughly the shape it asked for. Explicit
//     aspect_ratio and image_size win over a size.
//   - The picture comes back as JPEG in practice. The response reports the real
//     format and pixel dimensions rather than echoing what was asked for.

const (
	geminiImageGenerationAlt = "images/generations"
	geminiImageEditsAlt      = "images/edits"

	// geminiImageMaxReferenceImages is the upstream's ceiling on reference images
	// for one generation.
	geminiImageMaxReferenceImages = 14
	// geminiImageMaxReferenceBytes bounds one decoded reference image, matching the
	// upload limit of the images handler.
	geminiImageMaxReferenceBytes = 20 << 20
)

// geminiImageSizeTierPixels is the nominal pixel budget of each size tier, used to
// map a WIDTHxHEIGHT size onto the closest tier.
var geminiImageSizeTierPixels = map[string]float64{
	"512": 512 * 512,
	"1K":  1024 * 1024,
	"2K":  2048 * 2048,
	"4K":  4096 * 4096,
}

// geminiImageReferenceTypes are the image formats generateContent accepts inline.
var geminiImageReferenceTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/heic": {},
	"image/heif": {},
}

// geminiImageRequest is a parsed /v1/images/* request.
type geminiImageRequest struct {
	Prompt      string
	AspectRatio string
	ImageSize   string
	References  []geminiImageReference
}

// geminiImageReference is one reference frame, already base64 encoded.
type geminiImageReference struct {
	MimeType string
	Data     string
}

// isGeminiImageAlt reports whether a request arrived on one of the images
// endpoints.
func isGeminiImageAlt(alt string) bool {
	switch strings.TrimSpace(alt) {
	case geminiImageGenerationAlt, geminiImageEditsAlt:
		return true
	default:
		return false
	}
}

// parseGeminiImageRequest reads a /v1/images/* body. Edits must carry at least one
// reference image; generations ignore any that are present, as the OpenAI
// endpoint does.
func parseGeminiImageRequest(payload []byte, edit bool) (geminiImageRequest, error) {
	var parsed geminiImageRequest
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return parsed, fmt.Errorf("request body must be a JSON object")
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return parsed, fmt.Errorf("request body must be a JSON object")
	}

	parsed.Prompt = strings.TrimSpace(root.Get("prompt").String())
	if parsed.Prompt == "" {
		return parsed, fmt.Errorf("prompt is required")
	}
	if root.Get("stream").Bool() {
		return parsed, fmt.Errorf("streaming image generation is not supported for Gemini image models")
	}
	// There is nowhere to host a URL response, so only inline bytes are offered —
	// the same contract the Codex image path has.
	if format := strings.ToLower(strings.TrimSpace(root.Get("response_format").String())); format != "" && format != "b64_json" {
		return parsed, fmt.Errorf("only response_format=b64_json is supported for Gemini image models")
	}

	var err error
	if parsed.AspectRatio, err = normalizeGeminiAspectRatio(root.Get("aspect_ratio").String()); err != nil {
		return parsed, err
	}
	if parsed.ImageSize, err = normalizeGeminiImageSize(root.Get("image_size").String()); err != nil {
		return parsed, err
	}
	if size := strings.TrimSpace(root.Get("size").String()); size != "" && !strings.EqualFold(size, "auto") {
		width, height, ok := parseImageSizeDimensions(size)
		if !ok {
			return parsed, fmt.Errorf("size must be WIDTHxHEIGHT using positive integers, or auto")
		}
		if parsed.AspectRatio == "" {
			parsed.AspectRatio = nearestGeminiAspectRatio(width, height)
		}
		if parsed.ImageSize == "" {
			parsed.ImageSize = nearestGeminiImageSize(width, height)
		}
	}

	if !edit {
		return parsed, nil
	}
	// generateContent has no mask input. Ignoring one would hand back an edit of
	// the whole picture to a caller that asked for a region, so it is refused.
	if hasGeminiImageMask(root) {
		return parsed, fmt.Errorf("mask is not supported for Gemini image models; describe the area to change in the prompt instead")
	}
	if parsed.References, err = parseGeminiImageReferences(root); err != nil {
		return parsed, err
	}
	if len(parsed.References) == 0 {
		return parsed, fmt.Errorf("image is required for image edits")
	}
	return parsed, nil
}

// generateContentPayload renders the request as a Gemini generateContent body.
//
// responseModalities is left unset on purpose. The chat entrypoints have always
// sent these models a bare request and received the image, so this path asks the
// same way rather than introducing a variant nothing has exercised.
func (r geminiImageRequest) generateContentPayload() ([]byte, error) {
	parts := make([]map[string]any, 0, 1+len(r.References))
	parts = append(parts, map[string]any{"text": r.Prompt})
	for _, reference := range r.References {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{"mimeType": reference.MimeType, "data": reference.Data},
		})
	}
	body := map[string]any{
		"contents": []map[string]any{{"role": "user", "parts": parts}},
	}
	imageConfig := map[string]any{}
	if r.AspectRatio != "" {
		imageConfig["aspectRatio"] = r.AspectRatio
	}
	if r.ImageSize != "" {
		imageConfig["imageSize"] = r.ImageSize
	}
	if len(imageConfig) > 0 {
		body["generationConfig"] = map[string]any{"imageConfig": imageConfig}
	}
	return json.Marshal(body)
}

// normalizeGeminiAspectRatio accepts "W:H" (spaces allowed) and reduces it, so
// "32:18" is sent as the "16:9" upstream knows. Empty and "auto" leave the choice
// to the model.
func normalizeGeminiAspectRatio(raw string) (string, error) {
	value := strings.ReplaceAll(strings.TrimSpace(raw), " ", "")
	if value == "" || strings.EqualFold(value, "auto") {
		return "", nil
	}
	left, right, found := strings.Cut(value, ":")
	width, errWidth := strconv.Atoi(left)
	height, errHeight := strconv.Atoi(right)
	if !found || errWidth != nil || errHeight != nil || width <= 0 || height <= 0 {
		return "", fmt.Errorf("aspect_ratio must look like 16:9")
	}
	divisor := greatestCommonDivisor(width, height)
	return strconv.Itoa(width/divisor) + ":" + strconv.Itoa(height/divisor), nil
}

// normalizeGeminiImageSize maps the accepted spellings onto the canonical tier
// names. A "<n>K" outside the known tiers is passed through for upstream to judge;
// anything else is refused here, before an upstream round trip.
func normalizeGeminiImageSize(raw string) (string, error) {
	value := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
	switch value {
	case "", "AUTO":
		return "", nil
	case "512", "512P", "512PX", "0.5K":
		return "512", nil
	}
	if digits := strings.TrimSuffix(value, "K"); digits != value {
		if tier, err := strconv.Atoi(digits); err == nil && tier > 0 {
			return value, nil
		}
	}
	return "", fmt.Errorf("image_size must be one of %s", strings.Join(registry.GeminiImageSizes(), ", "))
}

// nearestGeminiAspectRatio picks the accepted ratio closest to width:height,
// compared on a log scale so 2:1 and 1:2 are equally far from 1:1.
func nearestGeminiAspectRatio(width, height int) string {
	target := math.Log(float64(width) / float64(height))
	best, bestDistance := "1:1", math.Inf(1)
	for _, ratio := range registry.GeminiImageAspectRatios() {
		left, right, _ := strings.Cut(ratio, ":")
		w, errW := strconv.Atoi(left)
		h, errH := strconv.Atoi(right)
		if errW != nil || errH != nil || w <= 0 || h <= 0 {
			continue
		}
		if distance := math.Abs(target - math.Log(float64(w)/float64(h))); distance < bestDistance {
			best, bestDistance = ratio, distance
		}
	}
	return best
}

// nearestGeminiImageSize picks the size tier whose pixel budget is closest to
// width*height on a log scale, so 1024x1024 is 1K and 2048x2048 is 2K.
func nearestGeminiImageSize(width, height int) string {
	target := math.Log(float64(width) * float64(height))
	best, bestDistance := "1K", math.Inf(1)
	for _, tier := range registry.GeminiImageSizes() {
		pixels, ok := geminiImageSizeTierPixels[tier]
		if !ok {
			continue
		}
		if distance := math.Abs(target - math.Log(pixels)); distance < bestDistance {
			best, bestDistance = tier, distance
		}
	}
	return best
}

// parseImageSizeDimensions splits "<width>x<height>".
func parseImageSizeDimensions(size string) (int, int, bool) {
	normalized := strings.NewReplacer("X", "x", "×", "x", "*", "x").Replace(strings.TrimSpace(size))
	left, right, found := strings.Cut(normalized, "x")
	if !found {
		return 0, 0, false
	}
	width, errWidth := strconv.Atoi(strings.TrimSpace(left))
	height, errHeight := strconv.Atoi(strings.TrimSpace(right))
	if errWidth != nil || errHeight != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// hasGeminiImageMask reports whether an edit request carries a mask in any of the
// shapes the images handlers produce.
func hasGeminiImageMask(root gjson.Result) bool {
	if file := root.Get("mask_file"); file.Exists() && file.Type != gjson.Null {
		return true
	}
	mask := root.Get("mask")
	switch {
	case !mask.Exists() || mask.Type == gjson.Null:
		return false
	case mask.Type == gjson.String:
		return strings.TrimSpace(mask.String()) != ""
	default:
		return strings.TrimSpace(mask.Get("image_url").String()) != "" || strings.TrimSpace(mask.Get("url").String()) != ""
	}
}

// parseGeminiImageReferences collects the reference frames of an edit request.
//
// Three shapes reach this point: image_files from a multipart upload, "image"
// (a string or a list) from JSON clients and the panel's model probe, and the
// "images" list of {image_url} objects the Codex path also reads. Only inline
// bytes are accepted. generateContent cannot fetch an arbitrary URL, and fetching
// it here would let any caller make this server request any address.
func parseGeminiImageReferences(root gjson.Result) ([]geminiImageReference, error) {
	references := make([]geminiImageReference, 0, 2)
	add := func(reference geminiImageReference) error {
		if len(references) >= geminiImageMaxReferenceImages {
			return fmt.Errorf("image edit supports at most %d reference images", geminiImageMaxReferenceImages)
		}
		references = append(references, reference)
		return nil
	}

	for _, upload := range root.Get("image_files").Array() {
		reference, err := geminiImageReferenceFromBase64(upload.Get("content_type").String(), upload.Get("data_base64").String())
		if err != nil {
			return nil, err
		}
		if err = add(reference); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"image", "images"} {
		value := root.Get(field)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		items := []gjson.Result{value}
		if value.IsArray() {
			items = value.Array()
		}
		for _, item := range items {
			uri := strings.TrimSpace(geminiImageReferenceURI(item))
			if uri == "" {
				continue
			}
			reference, err := geminiImageReferenceFromURI(uri)
			if err != nil {
				return nil, err
			}
			if err = add(reference); err != nil {
				return nil, err
			}
		}
	}
	return references, nil
}

// geminiImageReferenceURI reads a reference written as a bare string, as
// {"image_url": "..."}, as {"image_url": {"url": "..."}} or as {"url": "..."}.
func geminiImageReferenceURI(item gjson.Result) string {
	if item.Type == gjson.String {
		return item.String()
	}
	if url := item.Get("image_url"); url.Type == gjson.String {
		return url.String()
	}
	if url := item.Get("image_url.url"); url.Exists() {
		return url.String()
	}
	return item.Get("url").String()
}

func geminiImageReferenceFromURI(uri string) (geminiImageReference, error) {
	if !strings.HasPrefix(strings.ToLower(uri), "data:") {
		return geminiImageReference{}, fmt.Errorf("reference images must be uploaded or sent as base64 data URIs; remote URLs are not fetched")
	}
	header, data, found := strings.Cut(uri[len("data:"):], ",")
	if !found || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return geminiImageReference{}, fmt.Errorf("reference image data URI must be base64 encoded")
	}
	return geminiImageReferenceFromBase64(strings.TrimSuffix(header[:len(header)-len(";base64")], ";"), data)
}

// geminiImageReferenceFromBase64 validates one reference frame. The declared type
// is checked against the bytes when it is missing or generic, because multipart
// clients routinely send application/octet-stream.
//
// Unpadded base64 is accepted and re-encoded, so what reaches upstream is always
// the padded standard form generateContent expects.
func geminiImageReferenceFromBase64(contentType, data string) (geminiImageReference, error) {
	data = strings.TrimSpace(data)
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(data, "="))
	}
	if err != nil || len(decoded) == 0 {
		return geminiImageReference{}, fmt.Errorf("reference image is not valid base64 image data")
	}
	if len(decoded) > geminiImageMaxReferenceBytes {
		return geminiImageReference{}, fmt.Errorf("reference image exceeds %d MB", geminiImageMaxReferenceBytes>>20)
	}
	mimeType := strings.ToLower(strings.TrimSpace(contentType))
	if mediaType, _, found := strings.Cut(mimeType, ";"); found {
		mimeType = strings.TrimSpace(mediaType)
	}
	if mimeType == "image/jpg" {
		mimeType = "image/jpeg"
	}
	if _, ok := geminiImageReferenceTypes[mimeType]; !ok {
		mimeType = http.DetectContentType(decoded)
	}
	if _, ok := geminiImageReferenceTypes[mimeType]; !ok {
		return geminiImageReference{}, fmt.Errorf("reference images must be PNG, JPEG, WEBP, HEIC or HEIF")
	}
	return geminiImageReference{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(decoded)}, nil
}

// geminiImageResponseError carries a "no image" outcome. Its message names the
// request as the cause so the credential pool reads it as a request problem — a
// refused prompt — rather than as an unhealthy account to cool down.
func geminiImageResponseError(format string, args ...any) error {
	return statusErr{code: http.StatusBadRequest, msg: "invalid request: " + fmt.Sprintf(format, args...)}
}

// buildOpenAIImagesResponseFromGemini converts a Gemini generateContent response
// into an OpenAI images response.
func buildOpenAIImagesResponseFromGemini(payload []byte, created int64) ([]byte, error) {
	if !gjson.ValidBytes(payload) {
		return nil, statusErr{code: http.StatusBadGateway, msg: "upstream image response is not valid JSON"}
	}
	root := gjson.ParseBytes(payload)

	type imageItem struct {
		B64JSON string `json:"b64_json"`
	}
	items := make([]imageItem, 0, 1)
	texts := make([]string, 0, 1)
	outputFormat, size := "", ""
	finishReason := ""
	for _, candidate := range root.Get("candidates").Array() {
		if reason := strings.TrimSpace(candidate.Get("finishReason").String()); reason != "" {
			finishReason = reason
		}
		for _, part := range candidate.Get("content.parts").Array() {
			if part.Get("thought").Bool() {
				continue
			}
			inline := part.Get("inlineData")
			if !inline.Exists() {
				inline = part.Get("inline_data")
			}
			mimeType := strings.ToLower(strings.TrimSpace(inline.Get("mimeType").String()))
			if mimeType == "" {
				mimeType = strings.ToLower(strings.TrimSpace(inline.Get("mime_type").String()))
			}
			data := strings.TrimSpace(inline.Get("data").String())
			if strings.HasPrefix(mimeType, "image/") && data != "" {
				items = append(items, imageItem{B64JSON: data})
				if outputFormat == "" {
					outputFormat, size = describeGeminiImage(mimeType, data)
				}
				continue
			}
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		}
	}

	if len(items) == 0 {
		modelText := truncateGeminiImageText(strings.Join(texts, " "))
		switch {
		case root.Get("promptFeedback.blockReason").String() != "":
			return nil, geminiImageResponseError("the prompt was blocked by the upstream safety filter (%s)", root.Get("promptFeedback.blockReason").String())
		case finishReason != "" && !strings.EqualFold(finishReason, "STOP"):
			if modelText != "" {
				return nil, geminiImageResponseError("no image was generated (finish reason %s): %s", finishReason, modelText)
			}
			return nil, geminiImageResponseError("no image was generated (finish reason %s)", finishReason)
		case modelText != "":
			return nil, geminiImageResponseError("the model answered with text instead of an image: %s", modelText)
		default:
			return nil, statusErr{code: http.StatusBadGateway, msg: "upstream returned no image"}
		}
	}

	response := map[string]any{
		"created": created,
		"data":    items,
	}
	if outputFormat != "" {
		response["output_format"] = outputFormat
	}
	if size != "" {
		response["size"] = size
	}
	if usage := geminiImageUsage(root.Get("usageMetadata")); usage != nil {
		response["usage"] = usage
	}
	return json.Marshal(response)
}

// describeGeminiImage reports the output format and, when the header can be read,
// the pixel size of an inline image.
func describeGeminiImage(mimeType, data string) (string, string) {
	format := strings.TrimPrefix(mimeType, "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return format, ""
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return format, ""
	}
	return format, strconv.Itoa(config.Width) + "x" + strconv.Itoa(config.Height)
}

// geminiImageUsage maps usageMetadata onto the images usage block. Thinking tokens
// are output the account paid for, so they are counted with the candidates.
func geminiImageUsage(usage gjson.Result) map[string]any {
	if !usage.Exists() {
		return nil
	}
	input := usage.Get("promptTokenCount").Int()
	output := usage.Get("candidatesTokenCount").Int() + usage.Get("thoughtsTokenCount").Int()
	total := usage.Get("totalTokenCount").Int()
	if total == 0 {
		total = input + output
	}
	result := map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  total,
	}
	var textTokens, imageTokens int64
	for _, detail := range usage.Get("promptTokensDetails").Array() {
		switch strings.ToUpper(detail.Get("modality").String()) {
		case "TEXT":
			textTokens += detail.Get("tokenCount").Int()
		case "IMAGE":
			imageTokens += detail.Get("tokenCount").Int()
		}
	}
	if textTokens > 0 || imageTokens > 0 {
		result["input_tokens_details"] = map[string]any{"text_tokens": textTokens, "image_tokens": imageTokens}
	}
	return result
}

func truncateGeminiImageText(text string) string {
	const limit = 300
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
