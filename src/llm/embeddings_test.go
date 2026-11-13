package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveEmbeddings(t *testing.T, dims int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(req.Input) > embedBatchSize {
			t.Errorf("batch size %d exceeds cap", len(req.Input))
		}
		var resp embedResponse
		for i, in := range req.Input {
			vec := make([]float32, dims)
			vec[0] = float32(len(in))
			resp.Data = append(resp.Data, embedData{Index: i, Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestEmbedderBatchesAndKeepsOrder(t *testing.T) {
	srv, calls := serveEmbeddings(t, 3)
	e := NewEmbedder(Config{APIKey: "test-key", BaseURL: srv.URL}, "test-model", 3)

	texts := make([]string, embedBatchSize+6)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(texts))
	}
	if *calls != 2 {
		t.Errorf("made %d calls, want 2", *calls)
	}
	if vecs[3][0] != 4 || vecs[len(vecs)-1][0] != float32(len(texts[len(texts)-1])) {
		t.Errorf("vectors out of order")
	}
}

func TestEmbedderRejectsDimMismatch(t *testing.T) {
	srv, _ := serveEmbeddings(t, 2)
	e := NewEmbedder(Config{APIKey: "test-key", BaseURL: srv.URL}, "test-model", 3)
	if _, err := e.Embed(context.Background(), []string{"hello"}); err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}
