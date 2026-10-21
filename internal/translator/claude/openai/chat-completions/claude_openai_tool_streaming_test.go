package chat_completions

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func translateClaudeChatToolEvent(t *testing.T, state *any, event string) []string {
	t.Helper()
	return ConvertClaudeResponseToOpenAI(context.Background(), "claude-test", nil, nil, []byte("data: "+event), state)
}

func TestClaudeChatToolStream_StartDoesNotWaitForStop(t *testing.T) {
	startGate := make(chan struct{})
	stopGate := make(chan struct{})
	firstChunk := make(chan []string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var state any
		<-startGate
		firstChunk <- translateClaudeChatToolEvent(t, &state, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_slow","name":"slow_tool","input":{}}}`)
		<-stopGate
		translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":0}`)
	}()

	close(startGate)
	select {
	case chunks := <-firstChunk:
		if len(chunks) != 1 || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.id").String() != "toolu_slow" {
			t.Fatalf("start chunks = %v", chunks)
		}
	case <-time.After(time.Second):
		t.Fatal("tool metadata remained buffered while content_block_stop was gated")
	}
	close(stopGate)
	<-done
}

func TestClaudeChatToolStream_OrdinaryArgumentsAreIncrementalAndExact(t *testing.T) {
	var state any
	start := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"shell","input":{}}}`)
	if len(start) != 1 {
		t.Fatalf("start chunks = %d, want 1", len(start))
	}
	call := gjson.Get(start[0], "choices.0.delta.tool_calls.0")
	if call.Get("index").Int() != 2 || call.Get("id").String() != "toolu_2" || call.Get("type").String() != "function" || call.Get("function.name").String() != "shell" {
		t.Fatalf("start metadata = %s", call.Raw)
	}
	if call.Get("function.arguments").Exists() {
		t.Fatalf("start repeated arguments: %s", call.Raw)
	}

	var assembled string
	for _, partial := range []string{`{"command":`, `"printf ok"}`} {
		chunks := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":`+strconv.Quote(partial)+`}}`)
		if len(chunks) != 1 {
			t.Fatalf("argument chunks = %d for %q, want 1", len(chunks), partial)
		}
		delta := gjson.Get(chunks[0], "choices.0.delta.tool_calls.0")
		if delta.Get("index").Int() != 2 || delta.Get("id").Exists() || delta.Get("type").Exists() || delta.Get("function.name").Exists() {
			t.Fatalf("argument delta repeated metadata: %s", delta.Raw)
		}
		assembled += delta.Get("function.arguments").String()
	}
	if assembled != `{"command":"printf ok"}` {
		t.Fatalf("assembled arguments = %q", assembled)
	}
	if stop := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":2}`); len(stop) != 0 {
		t.Fatalf("stop duplicated ordinary arguments: %v", stop)
	}
}

func TestClaudeChatToolStream_EmptyArgumentsCompleteOnce(t *testing.T) {
	var state any
	translateClaudeChatToolEvent(t, &state, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"clock","input":{}}}`)
	stop := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":0}`)
	if len(stop) != 1 {
		t.Fatalf("stop chunks = %d, want 1", len(stop))
	}
	delta := gjson.Get(stop[0], "choices.0.delta.tool_calls.0")
	if delta.Get("function.arguments").String() != "{}" || delta.Get("id").Exists() || delta.Get("function.name").Exists() {
		t.Fatalf("empty completion = %s", delta.Raw)
	}
	if again := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":0}`); len(again) != 0 {
		t.Fatalf("repeated stop emitted duplicate: %v", again)
	}
}

func TestClaudeChatToolStream_MultipleToolsRemainIndependent(t *testing.T) {
	var state any
	for index, tool := range []struct{ id, name string }{{"toolu_a", "first"}, {"toolu_b", "second"}} {
		chunks := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"tool_use","id":`+strconv.Quote(tool.id)+`,"name":`+strconv.Quote(tool.name)+`,"input":{}}}`)
		if len(chunks) != 1 || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.index").Int() != int64(index) || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.id").String() != tool.id {
			t.Fatalf("tool %d start = %v", index, chunks)
		}
	}
	for index, partial := range []string{`{"a":1}`, `{"b":2}`} {
		chunks := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"input_json_delta","partial_json":`+strconv.Quote(partial)+`}}`)
		if len(chunks) != 1 || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.index").Int() != int64(index) || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String() != partial {
			t.Fatalf("tool %d delta = %v", index, chunks)
		}
	}
	for index := range 2 {
		if stop := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`); len(stop) != 0 {
			t.Fatalf("tool %d stop duplicated content: %v", index, stop)
		}
	}
}

func TestClaudeChatToolStream_IncompleteMetadataKeepsTerminalFallback(t *testing.T) {
	var state any
	if start := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_missing_name","input":{}}}`); len(start) != 0 {
		t.Fatalf("incomplete metadata streamed early: %v", start)
	}
	if delta := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`); len(delta) != 0 {
		t.Fatalf("arguments streamed without metadata: %v", delta)
	}
	stop := translateClaudeChatToolEvent(t, &state, `{"type":"content_block_stop","index":0}`)
	if len(stop) != 1 {
		t.Fatalf("terminal fallback chunks = %d, want 1", len(stop))
	}
	call := gjson.Get(stop[0], "choices.0.delta.tool_calls.0")
	if call.Get("id").String() != "toolu_missing_name" || call.Get("function.arguments").String() != "{}" {
		t.Fatalf("terminal fallback changed legacy payload: %s", call.Raw)
	}
}
