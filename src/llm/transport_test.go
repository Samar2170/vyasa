package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportRetriesRetryableStatus(t *testing.T) {
	var calls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, `{"error":{"message":"busy","code":429}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	retries := 1
	transport := NewTransport("test", Config{
		BaseURL:    server.URL,
		MaxRetries: &retries,
		Timeout:    time.Second,
	})

	var out struct {
		OK bool `json:"ok"`
	}
	if err := transport.DoJSON(
		context.Background(),
		http.MethodPost,
		server.URL,
		nil,
		map[string]string{"x": "y"},
		&out,
	); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if !out.OK {
		t.Fatal("response was not decoded")
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestTransportProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key","code":401}}`))
	}))
	defer server.Close()

	transport := NewTransport("test", Config{BaseURL: server.URL})

	var out any
	err := transport.DoJSON(
		context.Background(),
		http.MethodPost,
		server.URL,
		nil,
		map[string]string{},
		&out,
	)
	if err == nil {
		t.Fatal("expected error")
	}

	providerErr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.StatusCode != http.StatusUnauthorized || providerErr.Message != "invalid key" {
		t.Fatalf("error = %#v", providerErr)
	}
	if providerErr.Retryable {
		t.Fatal("401 should not be retryable")
	}
}
