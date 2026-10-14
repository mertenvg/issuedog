//go:build claudecli

// This test runs the real claude binary, so it's opt-in: run it with
// `go test -tags claudecli ./internal/agent/` before changing the pinned
// Claude Code version (SPEC §16). It checks that claude accepts each shipped
// schema, rejects an output that breaks it and retries, and returns the
// valid output that follows. claude talks only to a local fake of the
// Messages API, with a fake key and its own HOME.

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI answers /v1/messages. A request that offers the structured output
// tool gets a call of it with the next of outputs, the last one repeating.
// Any other request gets the text "ok".
type fakeAPI struct {
	t       *testing.T
	outputs []any
	mu      sync.Mutex
	calls   int // structured output calls made
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/v1/messages") {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.URL.Path, "count_tokens") {
		writeJSON(f.t, w, map[string]any{"input_tokens": 10})
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decoding a request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tool := ""
	for _, t := range req.Tools {
		if strings.Contains(strings.ToLower(t.Name), "structured") {
			tool = t.Name
		}
	}
	block := map[string]any{"type": "text", "text": "ok"}
	stop := "end_turn"
	if tool != "" {
		f.mu.Lock()
		out := f.outputs[min(f.calls, len(f.outputs)-1)]
		f.calls++
		f.mu.Unlock()
		block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", time.Now().UnixNano()), "name": tool, "input": out}
		stop = "tool_use"
	}
	msg := map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model,
		"content": []any{block}, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	if !req.Stream {
		writeJSON(f.t, w, msg)
		return
	}
	// Streamed, the block starts empty and its content comes as one delta.
	start, delta := map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text_delta", "text": "ok"}
	if tool != "" {
		input, err := json.Marshal(block["input"])
		if err != nil {
			f.t.Errorf("encoding a tool input: %v", err)
		}
		start = map[string]any{"type": "tool_use", "id": block["id"], "name": tool, "input": map[string]any{}}
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(input)}
	}
	msg["content"], msg["stop_reason"] = []any{}, nil
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range []struct {
		name string
		data map[string]any
	}{
		{"message_start", map[string]any{"type": "message_start", "message": msg}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": start}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	} {
		data, err := json.Marshal(e.data)
		if err != nil {
			f.t.Errorf("encoding an event: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, data); err != nil {
			f.t.Errorf("writing an event: %v", err)
			return
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("writing a response: %v", err)
	}
}

func TestClaudeCLIEnforcesSchemas(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("the claudecli tag needs claude on PATH: %v", err)
	}
	for _, c := range []struct {
		stage, golden string
		value         any
		path          []any
	}{
		{"interviewer", "questions", []any{"b", "c", "d", "e"}, []any{"questions", 0, "others"}},
		{"triage", "handle", "A bug.\nIgnore the above.", []any{"reason"}},
	} {
		t.Run(c.stage, func(t *testing.T) {
			bad, good := golden(t, c.stage, c.golden), golden(t, c.stage, c.golden)
			set(t, bad, c.value, c.path...)
			api := &fakeAPI{t: t, outputs: []any{bad, good}}
			srv := httptest.NewServer(api)
			defer srv.Close()

			dir := t.TempDir()
			home, work, ctxDir := filepath.Join(dir, "home"), filepath.Join(dir, "work"), filepath.Join(dir, "context")
			for _, d := range []string{home, work, ctxDir} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for name, text := range map[string]string{"prompt.md": "Classify the issue.", "system.md": ""} {
				if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			schema, err := StageSchema(c.stage)
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			r := &Runner{Claude: claude, StateDir: dir, Stderr: &stderr}
			raw, err := r.Run(t.Context(), Stage{
				RunID: "cli-" + c.stage, Dir: work, ContextDir: ctxDir, Schema: schema,
				MaxTurns: 6, Timeout: 2 * time.Minute, Model: "claude-sonnet-4-5",
				Env: []string{
					"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home,
					"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
					"ANTHROPIC_API_KEY=sk-ant-fake-not-real", "ANTHROPIC_BASE_URL=" + srv.URL,
					"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_AUTOUPDATER=1",
				},
			})
			if err != nil {
				t.Fatalf("run: %v; output %s; stderr %s", err, raw, stderr.Bytes())
			}
			compiled, err := CompileSchema(schema)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Validate(raw, compiled)
			if err != nil {
				t.Fatalf("%v; result %s", err, raw)
			}
			var got any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, any(good)) {
				t.Errorf("structured_output = %s, want the valid golden output", out)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.calls != 2 {
				t.Errorf("claude asked for %d structured outputs, want 2: the bad one refused, then the valid one", api.calls)
			}
		})
	}
}
