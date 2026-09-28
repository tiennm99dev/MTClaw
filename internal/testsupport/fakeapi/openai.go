package fakeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ToolCall is one function call a scripted Step's assistant message carries.
// Args is the tool's arguments, already JSON-encoded exactly as the model
// would emit them (e.g. `{"path":"notes.txt"}`) - openai-go's wire format
// carries function arguments as a JSON string, not a nested object, and
// this fake stays in wire-format terms rather than re-encoding a Go value.
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// Usage is one scripted Step's token accounting. A zero value is a valid
// script (most tests do not assert on usage), reported as 0/0.
type Usage struct {
	Prompt     int
	Completion int
}

// Step is one scripted chat-completions response: either Content (a final
// answer, finish_reason "stop") or ToolCalls (finish_reason "tool_calls") -
// a real completion can carry both, but every test in this repo scripts one
// or the other per turn, matching how internal/agent/loop.go actually
// branches on FinishReason.
type Step struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
}

// OpenAI is an httptest-backed fake of the OpenAI chat-completions endpoint:
// it answers every POST …/chat/completions with the next scripted Step, in
// order, and records each request's decoded JSON body so a test can assert
// what internal/provider/openai actually sent - most importantly, that a
// tool's result made it into the *next* request as a "tool" message,
// proving the agent loop's Observe step actually ran. It deliberately does
// not import internal/provider: everything here is plain wire-format JSON,
// which is the point of exercising the real openai-go client rather than
// internal/provider/mock.
type OpenAI struct {
	srv *httptest.Server

	mu       sync.Mutex
	steps    []Step
	next     int
	requests []map[string]any
}

// NewOpenAI starts the fake server scripted with steps, answered in order
// as successive /chat/completions calls arrive. Callers must Close it once
// done, typically via t.Cleanup.
func NewOpenAI(steps ...Step) *OpenAI {
	s := &OpenAI{steps: steps}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// BaseURL returns the value for openai.base_url: the fake server's URL plus
// the "/v1" path segment config.Default()'s own base_url includes, since
// internal/provider/openai passes cfg.BaseURL straight through to
// option.WithBaseURL with no path manipulation of its own.
func (s *OpenAI) BaseURL() string { return s.srv.URL + "/v1" }

// Close shuts the fake server down.
func (s *OpenAI) Close() { s.srv.Close() }

// Requests returns every /chat/completions request body received so far,
// decoded from JSON, in arrival order.
func (s *OpenAI) Requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.requests))
	copy(out, s.requests)
	return out
}

// handle answers any path ending "/chat/completions" (matching whatever
// base_url a test configures) with the next scripted Step. Running out of
// steps is a test-authoring bug, not a real provider failure, so it answers
// with an HTTP 500 whose body names the problem in plain text instead of
// hanging or panicking - a mis-scripted test then fails with an obvious
// message the moment it makes one request too many, rather than blocking
// until its own timeout.
func (s *OpenAI) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.Error(w, "fakeapi.OpenAI: unsupported path "+r.URL.Path, http.StatusNotFound)
		return
	}

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.requests = append(s.requests, body)
	idx := s.next
	s.next++
	var step Step
	haveStep := idx < len(s.steps)
	if haveStep {
		step = s.steps[idx]
	}
	total := len(s.steps)
	s.mu.Unlock()

	if !haveStep {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf(
					"fakeapi.OpenAI: request %d exceeds the %d scripted step(s) this test provided - "+
						"the test under-scripted fakeapi.NewOpenAI's steps", idx+1, total,
				),
				"type": "fakeapi_test_authoring_bug",
			},
		})
		return
	}

	model, _ := body["model"].(string)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(chatCompletionResponse(model, step))
}

// chatCompletionResponse renders step as a full OpenAI chat-completion
// object: id, object, created, model, one choice (message + finish_reason),
// and usage - the shape internal/provider/openai/chat.go's fromSDK decodes.
func chatCompletionResponse(model string, step Step) map[string]any {
	message := map[string]any{"role": "assistant"}
	finishReason := "stop"

	if len(step.ToolCalls) > 0 {
		finishReason = "tool_calls"
		calls := make([]map[string]any, 0, len(step.ToolCalls))
		for _, tc := range step.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Args,
				},
			})
		}
		message["tool_calls"] = calls
		// A tool-calling turn may still carry no text content; OpenAI's own
		// wire format uses null, not an absent key, for that case.
		if step.Content != "" {
			message["content"] = step.Content
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = step.Content
	}

	return map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]any{
			"prompt_tokens":     step.Usage.Prompt,
			"completion_tokens": step.Usage.Completion,
			"total_tokens":      step.Usage.Prompt + step.Usage.Completion,
		},
	}
}
