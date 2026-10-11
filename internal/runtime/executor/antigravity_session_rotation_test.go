package executor

import (
	"context"
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

// What the upstream answers once a session's server-side input passes the
// 1,048,576-token ceiling.
const tokenWallBody = `{"error":{"code":400,"message":"The input token count (1048577) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`

type upstreamReply struct {
	status int
	body   string
}

// upstreamReplies answers successive requests with the given replies, repeating
// the last one, and keeps every request body so a test can read the session id
// each attempt was sent on.
func upstreamReplies(t *testing.T, replies ...upstreamReply) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, body)
		mu.Unlock()
		if n >= len(replies) {
			n = len(replies) - 1
		}
		if replies[n].status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(replies[n].status)
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = w.Write([]byte(replies[n].body))
	}))
	t.Cleanup(server.Close)
	return server, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

func upstreamSessionIDs(bodies [][]byte) []string {
	ids := make([]string, len(bodies))
	for i, body := range bodies {
		ids[i] = gjson.GetBytes(body, "request.sessionId").String()
	}
	return ids
}

// A request that hits the token wall is sent again on the next session
// generation. The default config allows a single attempt, so the retry also
// shows that a rotation does not spend one.
func TestAntigravityStreamRotatesSessionPastTokenWall(t *testing.T) {
	server, requests := upstreamReplies(t,
		upstreamReply{http.StatusBadRequest, tokenWallBody},
		upstreamReply{http.StatusOK, sseThought + sseCall + sseStop},
	)
	events := runMalformedCallStream(t, server.URL)

	bodies := requests()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
	ids := upstreamSessionIDs(bodies)
	if want := generateStableSessionID(bodies[0], 0); ids[0] != want {
		t.Fatalf("first attempt session = %q, want generation 0 %q", ids[0], want)
	}
	if want := generateStableSessionID(bodies[0], 1); ids[1] != want {
		t.Fatalf("retry session = %q, want generation 1 %q", ids[1], want)
	}
	if n := len(eventsOfType(events, "response.completed")); n != 1 {
		t.Fatalf("response.completed events = %d, want 1", n)
	}
}

// After a rotation, the malformed-call re-request stays on the rotated session:
// generation 0 would land on the exhausted session and fail with 400 again.
func TestAntigravityStreamMalformedRetryStaysOnRotatedSession(t *testing.T) {
	server, requests := upstreamReplies(t,
		upstreamReply{http.StatusBadRequest, tokenWallBody},
		upstreamReply{http.StatusOK, sseThought + sseMalformed},
		upstreamReply{http.StatusOK, sseThought + sseCall + sseStop},
	)
	events := runMalformedCallStream(t, server.URL)

	ids := upstreamSessionIDs(requests())
	if len(ids) != 3 {
		t.Fatalf("upstream calls = %d, want 3", len(ids))
	}
	if ids[1] == ids[0] || ids[2] != ids[1] {
		t.Fatalf("sessions = %q, want the re-request on the rotated session %q", ids, ids[1])
	}
	completed := eventsOfType(events, "response.completed")
	if len(completed) != 1 || !strings.Contains(gjson.Get(completed[0], "response.output").Raw, `"function_call"`) {
		t.Fatalf("want one completed response carrying the function call, got %d: %v", len(completed), completed)
	}
}

func TestAntigravityNonStreamMalformedRetryStaysOnRotatedSession(t *testing.T) {
	server, requests := upstreamReplies(t,
		upstreamReply{http.StatusBadRequest, tokenWallBody},
		upstreamReply{http.StatusOK, sseThought + sseMalformed},
		upstreamReply{http.StatusOK, sseThought + sseCall + sseStop},
	)
	resp, err := NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: []byte(strings.Replace(malformedCallResponsesRequest, `"stream":true`, `"stream":false`, 1)),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	ids := upstreamSessionIDs(requests())
	if len(ids) != 3 {
		t.Fatalf("upstream calls = %d, want 3", len(ids))
	}
	if ids[1] == ids[0] || ids[2] != ids[1] {
		t.Fatalf("sessions = %q, want the re-request on the rotated session %q", ids, ids[1])
	}
	if !strings.Contains(gjson.GetBytes(resp.Payload, "output").Raw, `"function_call"`) {
		t.Fatalf("output has no function_call: %s", resp.Payload)
	}
}

// A request whose own input exceeds the ceiling keeps failing on every session;
// it gives up after antigravitySessionBumpMaxAttempts rotations instead of looping.
func TestAntigravitySessionRotationIsBounded(t *testing.T) {
	server, requests := upstreamReplies(t, upstreamReply{http.StatusBadRequest, tokenWallBody})
	_, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: []byte(malformedCallResponsesRequest),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want the upstream 400")
	}
	if got, want := len(requests()), 1+antigravitySessionBumpMaxAttempts; got != want {
		t.Fatalf("upstream calls = %d, want %d", got, want)
	}
}
