package store

import (
	"context"
	"os"
	"testing"
	"time"

	"vyasa/src/models"
)

// TestSessionLogsRoundTrip verifies the session_logs schema and the
// insert/read queries against a live PostgreSQL instance. Skipped unless
// POSTGRES_DSN is set, so plain `go test ./...` stays offline.
func TestSessionLogsRoundTrip(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("integration: POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := New(ctx, dsn, os.Getenv("VYASA_TEST_SCHEMA"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	sessionID := "test-" + time.Now().Format("20060102150405")
	events := []models.SessionEvent{
		{SessionID: sessionID, Seq: 0, Kind: "session_start",
			Payload: map[string]any{"command": "test", "symbol": "RELIANCE"}},
		{SessionID: sessionID, Seq: 1, RunID: "r1", ParentID: "", Agent: "analyst",
			Kind: "run_start", Step: 0,
			Payload: map[string]any{"text": "do the thing"}},
		{SessionID: sessionID, Seq: 2, RunID: "r1", Agent: "analyst",
			Kind: "tool_call", Step: 1,
			Payload: map[string]any{"tool": map[string]any{"name": "rag_search", "arguments": map[string]any{"query": "capex"}}}},
		{SessionID: sessionID, Seq: 3, RunID: "r1", Agent: "analyst",
			Kind: "session_end", Payload: map[string]any{"status": "completed"}},
	}
	if err := s.InsertSessionEvents(ctx, events); err != nil {
		t.Fatalf("InsertSessionEvents: %v", err)
	}

	rows, err := s.SessionEvents(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(rows) != len(events) {
		t.Fatalf("got %d rows, want %d", len(rows), len(events))
	}
	for i, r := range rows {
		if r.Kind != events[i].Kind || r.Agent != events[i].Agent || r.Seq != events[i].Seq {
			t.Errorf("row %d = %+v, want kind=%s agent=%s seq=%d", i, r, events[i].Kind, events[i].Agent, events[i].Seq)
		}
	}
	// jsonb payloads survive the round trip.
	tool, ok := rows[2].Payload["tool"].(map[string]any)
	if !ok || tool["name"] != "rag_search" {
		t.Errorf("row 2 payload = %#v", rows[2].Payload)
	}
	args, ok := tool["arguments"].(map[string]any)
	if !ok || args["query"] != "capex" {
		t.Errorf("arguments payload = %#v", tool["arguments"])
	}
	if rows[3].Payload["status"] != "completed" {
		t.Errorf("session_end payload = %#v", rows[3].Payload)
	}

	// Cleanup so repeated runs don't accumulate test rows.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM session_logs WHERE session_id = $1`, sessionID); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}
