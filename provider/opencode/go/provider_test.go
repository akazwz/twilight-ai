package opencodego_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	opencodego "github.com/felinics/twilight/provider/opencode/go"
	"github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"
)

var routes = []struct {
	model    string
	protocol opencodego.Protocol
	path     string
}{
	{"glm-5.2", opencodego.ProtocolCompletions, "/chat/completions"},
	{"gpt-5.6-luna", opencodego.ProtocolResponses, "/responses"},
	{"minimax-m2.7", opencodego.ProtocolMessages, "/messages"},
}

func TestGenerationAndToolContinuations(t *testing.T) {
	for _, route := range routes {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route.protocol, stream), func(t *testing.T) {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					step := requests.Add(1)
					if r.URL.Path != "/zen/go/v1"+route.path || r.Method != http.MethodPost {
						t.Errorf("request = %s %s", r.Method, r.URL.Path)
					}
					if r.Header.Get(opencodego.SessionHeader) != "conversation-1" || r.Header.Get("User-Agent") != "test-agent/1.0" || r.Header.Get("X-Provider") != "kept" {
						t.Errorf("headers = %v", r.Header)
					}
					if route.protocol == opencodego.ProtocolMessages {
						if r.Header.Get("x-api-key") != "key" || r.Header.Get("anthropic-version") == "" {
							t.Errorf("Anthropic auth = %v", r.Header)
						}
					} else if r.Header.Get("Authorization") != "Bearer key" {
						t.Errorf("OpenAI auth = %v", r.Header)
					}
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					if string(body["model"]) != fmt.Sprintf("%q", route.model) {
						t.Errorf("model = %s", body["model"])
					}
					if stream && string(body["stream"]) != "true" {
						t.Error("missing stream flag")
					}
					if len(body["tools"]) == 0 {
						t.Error("tools were dropped")
					}
					checkProtocolRequest(t, route.protocol, step, body)
					reply(w, route.protocol, route.model, stream, step == 1)
				}))
				defer srv.Close()
				p := opencodego.New(opencodego.WithAPIKey("key"), opencodego.WithBaseURL(srv.URL+"/zen/go/v1"), opencodego.WithHeaders(map[string]string{"User-Agent": "test-agent/1.0", "X-Provider": "kept"}))
				model := p.ChatModel(route.model)
				ctx := sdk.WithRequestHeaders(context.Background(), map[string]string{opencodego.SessionHeader: "conversation-1"})
				options := []sdk.GenerateOption{
					sdk.WithModel(model), sdk.WithMessages([]sdk.Message{sdk.UserMessage("hi")}), sdk.WithMaxSteps(3), sdk.WithReasoningEffort("high"),
					sdk.WithTools([]sdk.Tool{{Name: "lookup", Parameters: &jsonschema.Schema{Type: "object"}, Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) { return "found", nil }}}),
				}
				var result *sdk.GenerateResult
				var err error
				if stream {
					var sr *sdk.StreamResult
					sr, err = sdk.StreamText(ctx, options...)
					if err == nil {
						result, err = sr.ToResult()
					}
				} else {
					result, err = sdk.GenerateTextResult(ctx, options...)
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.Text != "done" || result.FinishReason != sdk.FinishReasonStop {
					t.Fatalf("result = %+v", result)
				}
				if requests.Load() != 2 {
					t.Errorf("requests = %d, want 2", requests.Load())
				}
				if model.Provider != p || model.ID != route.model {
					t.Error("caller model was mutated")
				}
			})
		}
	}
}

func TestConcurrentSessionIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		session := r.Header.Get(opencodego.SessionHeader)
		if session == "" || !strings.Contains(string(body), fmt.Sprintf("%q", session)) {
			t.Errorf("session %q does not belong to request %s", session, body)
		}
		for _, route := range routes {
			if route.model == payload.Model {
				reply(w, route.protocol, payload.Model, false, false)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := opencodego.New(opencodego.WithBaseURL(srv.URL), opencodego.WithHeaders(map[string]string{"User-Agent": "test-agent/1.0"}))
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			session := fmt.Sprintf("session-%d", i)
			headers := map[string]string{opencodego.SessionHeader: session}
			ctx := sdk.WithRequestHeaders(context.Background(), headers)
			headers[opencodego.SessionHeader] = "mutated"
			_, err := sdk.GenerateText(ctx, sdk.WithModel(p.ChatModel(routes[i%len(routes)].model)), sdk.WithMessages([]sdk.Message{sdk.UserMessage(session)}))
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestDiscoveryAndProbes(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/models" {
			io.WriteString(w, `{"data":[{"id":"glm-5.2"},{"id":"future-model"}]}`)
			return
		}
		if r.Header.Get(opencodego.SessionHeader) == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		for _, route := range routes {
			if body.Model == route.model {
				if r.URL.Path != route.path {
					t.Errorf("probe path = %s, want %s", r.URL.Path, route.path)
				}
				reply(w, route.protocol, route.model, false, false)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := opencodego.New(opencodego.WithBaseURL(srv.URL))
	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %v, err = %v", models, err)
	}
	for _, model := range models {
		if model.Provider != p {
			t.Error("discovered model not bound to Go provider")
		}
	}
	status := p.Test(context.Background())
	if status.Status != sdk.ProviderStatusOK || !strings.Contains(status.Message, "TestModel") {
		t.Fatalf("public catalog test = %+v", status)
	}
	if _, err := p.TestModel(context.Background(), "glm-5.2"); err == nil {
		t.Fatal("rejected session was treated as a working model")
	}
	ctx := sdk.WithRequestHeaders(context.Background(), map[string]string{opencodego.SessionHeader: "probe-session"})
	for _, route := range routes {
		result, err := p.TestModel(ctx, route.model)
		if err != nil || !result.Supported {
			t.Fatalf("probe %s = %+v, %v", route.model, result, err)
		}
	}
	if requests.Load() != 6 {
		t.Fatalf("requests = %d, want 6", requests.Load())
	}
}

func TestExplicitRoutesAndInputValidation(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		reply(w, opencodego.ProtocolMessages, "new-model", false, false)
	}))
	defer srv.Close()
	overrides := map[string]opencodego.Protocol{"new-model": opencodego.ProtocolMessages, "glm-5.2": opencodego.ProtocolMessages, "bad-model": "invalid"}
	option := opencodego.WithModelProtocols(overrides)
	overrides["new-model"] = opencodego.ProtocolResponses
	p := opencodego.New(opencodego.WithBaseURL(srv.URL), option)
	other := opencodego.New(option, opencodego.WithModelProtocols(map[string]opencodego.Protocol{"new-model": opencodego.ProtocolResponses}))
	if protocol, _ := p.ProtocolForModel("new-model"); protocol != opencodego.ProtocolMessages {
		t.Fatal("route map was not copied")
	}
	if protocol, _ := other.ProtocolForModel("new-model"); protocol != opencodego.ProtocolResponses {
		t.Fatal("override not applied")
	}
	if protocol, _ := p.ProtocolForModel("glm-5.2"); protocol != opencodego.ProtocolMessages {
		t.Fatal("built-in route not overridden")
	}
	for _, id := range []string{"", "unknown", "glm-future", "bad-model", "opencode-go/glm-5.2"} {
		params := sdk.GenerateParams{Model: p.ChatModel(id)}
		if _, err := p.DoGenerate(context.Background(), params); err == nil {
			t.Errorf("generated unknown model %q", id)
		}
		if _, err := p.DoStream(context.Background(), params); err == nil {
			t.Errorf("streamed unknown model %q", id)
		}
		if _, err := p.TestModel(context.Background(), id); err == nil {
			t.Errorf("probed unknown model %q", id)
		}
	}
	if _, err := p.DoGenerate(context.Background(), sdk.GenerateParams{}); err == nil {
		t.Error("nil model accepted")
	}
	if _, err := p.DoStream(context.Background(), sdk.GenerateParams{}); err == nil {
		t.Error("nil streaming model accepted")
	}
	if requests.Load() != 0 {
		t.Error("invalid inputs reached server")
	}
	_, err := p.DoGenerate(context.Background(), sdk.GenerateParams{Model: p.ChatModel("new-model"), Messages: []sdk.Message{sdk.UserMessage("hi")}})
	if err != nil || requests.Load() != 1 {
		t.Fatalf("explicit model: requests = %d, err = %v", requests.Load(), err)
	}
}

// Replies exercise real protocol parsers and real tool-loop serialization.
func reply(w http.ResponseWriter, protocol opencodego.Protocol, model string, stream, tool bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	event := func(name, data string) {
		var body map[string]any
		_ = json.Unmarshal([]byte(data), &body)
		body["type"] = name
		encoded, _ := json.Marshal(body)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
	}
	switch protocol {
	case opencodego.ProtocolCompletions:
		finish := "stop"
		message := `{"role":"assistant","content":"done"}`
		if tool {
			finish = "tool_calls"
			message = `{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
		}
		if stream {
			fmt.Fprintf(w, "data: {\"id\":\"resp-1\",\"model\":%q,\"choices\":[{\"delta\":%s,\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", model, message, finish)
		} else {
			fmt.Fprintf(w, `{"id":"resp-1","model":%q,"choices":[{"message":%s,"finish_reason":%q}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`, model, message, finish)
		}
	case opencodego.ProtocolResponses:
		output := `{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"done"}]}`
		if tool {
			output = `{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup","arguments":"{}"}`
		}
		if stream {
			event("response.created", fmt.Sprintf(`{"response":{"id":"resp-1","model":%q}}`, model))
			event("response.output_item.added", fmt.Sprintf(`{"output_index":0,"item":%s}`, output))
			if !tool {
				event("response.output_text.delta", `{"item_id":"msg-1","delta":"done"}`)
			}
			event("response.output_item.done", fmt.Sprintf(`{"output_index":0,"item":%s}`, output))
			event("response.completed", `{"response":{"status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}`)
		} else {
			fmt.Fprintf(w, `{"id":"resp-1","model":%q,"status":"completed","output":[%s],"usage":{"input_tokens":4,"output_tokens":2}}`, model, output)
		}
	case opencodego.ProtocolMessages:
		finish := "end_turn"
		content := `{"type":"text","text":"done"}`
		if tool {
			finish = "tool_use"
			content = `{"type":"tool_use","id":"call-1","name":"lookup","input":{}}`
		}
		if stream {
			event("message_start", fmt.Sprintf(`{"message":{"id":"msg-1","model":%q,"role":"assistant","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}`, model))
			if tool {
				event("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"call-1","name":"lookup"}}`)
				event("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
			} else {
				event("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`)
				event("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"done"}}`)
			}
			event("content_block_stop", `{"index":0}`)
			event("message_delta", fmt.Sprintf(`{"delta":{"stop_reason":%q},"usage":{"output_tokens":2}}`, finish))
			event("message_stop", `{}`)
		} else {
			fmt.Fprintf(w, `{"id":"msg-1","type":"message","model":%q,"role":"assistant","content":[%s],"stop_reason":%q,"usage":{"input_tokens":4,"output_tokens":2}}`, model, content, finish)
		}
	}
}

func TestUpstreamErrorsAndCancellation(t *testing.T) {
	for _, route := range routes {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", route.protocol, status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "upstream rejected request", status) }))
				defer srv.Close()
				p := opencodego.New(opencodego.WithBaseURL(srv.URL))
				params := sdk.GenerateParams{Model: p.ChatModel(route.model), Messages: []sdk.Message{sdk.UserMessage("hi")}}
				if _, err := p.DoGenerate(context.Background(), params); err == nil {
					t.Error("generation ignored upstream error")
				}
				sr, err := p.DoStream(context.Background(), params)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sr.ToResult(); err == nil {
					t.Error("stream ignored upstream error")
				}
				if _, err := p.TestModel(context.Background(), route.model); err == nil {
					t.Error("probe ignored upstream error")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := p.DoGenerate(ctx, params); err == nil {
					t.Error("generation ignored cancellation")
				}
			})
		}
	}
}

func checkProtocolRequest(t *testing.T, protocol opencodego.Protocol, step int32, body map[string]json.RawMessage) {
	t.Helper()
	switch protocol {
	case opencodego.ProtocolCompletions:
		if len(body["messages"]) == 0 || string(body["reasoning_effort"]) != `"high"` {
			t.Errorf("completions body = %s", body)
		}
		if step == 2 && !strings.Contains(string(body["messages"]), `"role":"tool"`) {
			t.Error("missing tool result")
		}
	case opencodego.ProtocolResponses:
		if len(body["input"]) == 0 || len(body["reasoning"]) == 0 {
			t.Errorf("responses body = %s", body)
		}
		if step == 2 && !strings.Contains(string(body["input"]), "function_call_output") {
			t.Error("missing tool result")
		}
	case opencodego.ProtocolMessages:
		if len(body["messages"]) == 0 || len(body["max_tokens"]) == 0 || len(body["output_config"]) == 0 {
			t.Errorf("messages body = %s", body)
		}
		if step == 2 && !strings.Contains(string(body["messages"]), "tool_result") {
			t.Error("missing tool result")
		}
	}
}
