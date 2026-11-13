package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vyasa/src/llm"
	"vyasa/src/models"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short untouched", "hello world", 11, "hello world"},
		{"exact length untouched", "hello", 5, "hello"},
		{"empty", "", 10, ""},
		{"zero n", "hello", 0, ""},
		{"negative n", "hello", -1, ""},
		{"cuts at word boundary", "alpha beta gamma delta", 13, "alpha beta"},
		{"backoff too shallow cuts mid-word", "hello world", 10, "hello worl"},
		{"no space in back half", "abcdefghijklmnop", 12, "abcdefghijkl"},
		{"word boundary before newline", "alpha\nbeta gamma", 12, "alpha\nbeta"},
		{"unicode runes not bytes", "héllo wörld ünïcode", 6, "héllo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateRunes(tt.in, tt.n); got != tt.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestRerankTruncatesDocs(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		got = req.Documents
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	long := "word "
	for len([]rune(long)) < rerankDocRunes*2 {
		long += "word "
	}
	hits := []models.RAGHit{
		{RAGChunk: models.RAGChunk{Text: "short chunk"}},
		{RAGChunk: models.RAGChunk{Text: long}},
	}
	ix := NewIndexer(nil, nil, "", 0).WithReranker(
		llm.NewReranker(llm.Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b"), 32)
	if _, err := ix.rerank(context.Background(), "query", hits, 1); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if got[0] != "short chunk" {
		t.Errorf("short doc mutated: %q", got[0])
	}
	if n := len([]rune(got[1])); n > rerankDocRunes {
		t.Errorf("long doc sent with %d runes, cap is %d", n, rerankDocRunes)
	}
}
