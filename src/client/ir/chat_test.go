package ir

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"vyasa/src/llm"
)

func TestChatJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("authorization = %q, want Bearer key", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var req chatRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request: %v", err)
			return
		}
		if req.Model != "test-model" || len(req.Messages) != 2 ||
			req.Messages[0].Role != "system" || req.Messages[0].Content != "sys" ||
			req.Messages[1].Role != "user" || req.Messages[1].Content != "usr" {
			t.Errorf("request = %+v", req)
		}
		// Fenced JSON reply, as models often wrap their answers.
		w.Header().Set("Content-Type", "application/json")
		reply := map[string]any{
			"choices": []map[string]any{{
				"message": chatMessage{
					Role:    "assistant",
					Content: "```json\n{\"annual_report\": 3}\n```",
				},
			}},
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	cfg := llm.Config{APIKey: "key", BaseURL: srv.URL, DefaultModel: "test-model"}
	c := &Client{llm: newLLMChat("openrouter", cfg)}
	var out map[string]any
	if err := c.chatJSON(context.Background(), "sys", "usr", &out); err != nil {
		t.Fatalf("chatJSON: %v", err)
	}
	if out["annual_report"] != float64(3) {
		t.Fatalf("out = %v", out)
	}
}

func TestChatJSONErrors(t *testing.T) {
	var out map[string]any

	// No LLM handle configured.
	c := &Client{}
	if err := c.chatJSON(context.Background(), "", "", &out); err == nil {
		t.Fatal("expected error when llm is not configured")
	}

	// In-body provider error on a 200 reply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"quota reached","code":429}}`))
	}))
	defer srv.Close()
	cfg := llm.Config{APIKey: "key", BaseURL: srv.URL, DefaultModel: "m"}
	c = &Client{llm: newLLMChat("openrouter", cfg)}
	if err := c.chatJSON(context.Background(), "", "", &out); err == nil {
		t.Fatal("expected error for in-body provider error")
	}

	// Non-JSON reply.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not json"}}]}`))
	}))
	defer srv2.Close()
	cfg.BaseURL = srv2.URL
	c = &Client{llm: newLLMChat("openrouter", cfg)}
	if err := c.chatJSON(context.Background(), "", "", &out); err == nil {
		t.Fatal("expected error for non-JSON reply")
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := map[string]string{
		`{"a": 1}`:                   `{"a": 1}`,
		"  {\"a\": 1}  ":             `{"a": 1}`,
		"```json\n{\"a\": 1}\n```":   `{"a": 1}`,
		"```\n{\"a\": 1}\n```":       `{"a": 1}`,
		"```json\n{\"a\": 1}\n```\n": `{"a": 1}`,
	}
	for in, want := range cases {
		if got := stripCodeFence(in); got != want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}
