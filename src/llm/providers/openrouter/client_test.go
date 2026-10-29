package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"vyasa/src/llm"
)

func TestComplete(t *testing.T) {
	var got chatRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen-1","model":"test","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer server.Close()

	client, err := New(Config{Config: llm.Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := client.Complete(context.Background(), llm.Request{
		Model: "test",
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Kind: llm.PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Text() != "hi" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
	if got.Model != "test" {
		t.Fatalf("model = %q", got.Model)
	}
	if got.Messages[0].Content != "hello" {
		t.Fatalf("content = %#v", got.Messages[0].Content)
	}
}

func TestCompleteDetailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen-1","model":"test","provider":"openai","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop","native_finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer server.Close()

	client, err := New(Config{Config: llm.Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := client.CompleteDetailed(context.Background(), llm.Request{
		Model: "test",
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Kind: llm.PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("CompleteDetailed: %v", err)
	}
	if resp.Provider != "openai" || resp.NativeFinishReason != "stop" {
		t.Fatalf("extras = %#v", resp)
	}
}

func TestStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !req.Stream {
			t.Error("stream = false")
		}
		if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Error("include_usage = false")
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("flusher unavailable")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		_, _ = w.Write([]byte("data: {\"id\":\"gen-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n"))
		flusher.Flush()

		_, _ = w.Write([]byte("data: {\"id\":\"gen-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		flusher.Flush()

		_, _ = w.Write([]byte("data: {\"id\":\"gen-1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n"))
		flusher.Flush()

		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := New(Config{Config: llm.Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := client.Stream(context.Background(), llm.Request{
		Model: "m",
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Kind: llm.PartText, Text: "hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	acc := llm.NewAccumulator()
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		acc.Add(chunk)
	}

	got := acc.Result()
	if got.Message.Text() != "Hello" {
		t.Fatalf("text = %q", got.Message.Text())
	}
	if got.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %#v", got.Usage)
	}
	if got.FinishReason != llm.FinishStop {
		t.Fatalf("finish = %q", got.FinishReason)
	}
}
