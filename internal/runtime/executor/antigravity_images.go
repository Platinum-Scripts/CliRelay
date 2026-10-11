package executor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// executeOpenAIImages serves /v1/images/* for an Antigravity Gemini image model.
//
// The images request is rewritten into a Gemini generateContent body and sent down
// the same non-streaming path chat requests take. Token refresh, host fallback,
// retries, request recording and usage reporting therefore behave exactly as they
// do for the chat entrypoints that already serve these models; only the two ends
// differ — an images request in, an images response out.
func (e *AntigravityExecutor) executeOpenAIImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if !registry.IsGeminiImageModel(req.Model) {
		return cliproxyexecutor.Response{}, statusErr{
			code: http.StatusBadRequest,
			msg:  fmt.Sprintf("invalid request: model %q does not generate images on Antigravity", req.Model),
		}
	}
	parsed, err := parseGeminiImageRequest(req.Payload, opts.Alt == geminiImageEditsAlt)
	if err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: "invalid request: " + err.Error()}
	}
	body, err := parsed.generateContentPayload()
	if err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: "invalid request: " + err.Error()}
	}

	geminiFormat := sdktranslator.FromString("gemini")
	innerOpts := opts
	// The images alt must not reach buildRequest, which would put it in the
	// upstream URL as $alt=images/generations.
	innerOpts.Alt = ""
	innerOpts.Stream = false
	innerOpts.SourceFormat = geminiFormat
	innerOpts.OriginalRequest = body
	resp, err := e.executeViaStreamEndpoint(ctx, auth, cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  body,
		Format:   geminiFormat,
		Metadata: req.Metadata,
	}, innerOpts)
	if err != nil {
		return resp, err
	}

	payload, err := buildOpenAIImagesResponseFromGemini(resp.Payload, time.Now().Unix())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: payload, Headers: resp.Headers}, nil
}
