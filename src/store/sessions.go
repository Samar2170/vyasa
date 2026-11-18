package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"vyasa/src/models"
)

// InsertSessionEvents persists one session's agent events in a single
// round trip, preserving the caller-assigned seq order. Payload maps are
// encoded as jsonb; a payload that fails to marshal degrades to an error
// marker rather than dropping the event.
func (s *Store) InsertSessionEvents(ctx context.Context, events []models.SessionEvent) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for i := range events {
		payload, err := json.Marshal(events[i].Payload)
		if err != nil {
			payload = []byte(fmt.Sprintf(`{"error":"payload marshal failed: %s"}`, jsonQuote(err.Error())))
		}
		batch.Queue(`INSERT INTO session_logs
			(session_id, seq, run_id, parent_id, agent, kind, step, payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`,
			events[i].SessionID, events[i].Seq, events[i].RunID, events[i].ParentID,
			events[i].Agent, events[i].Kind, events[i].Step, string(payload))
	}
	if err := s.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: insert %d session events: %w", len(events), err)
	}
	return nil
}

// SessionEvents returns one session's flat event log, ordered by seq.
func (s *Store) SessionEvents(ctx context.Context, sessionID string) ([]models.SessionEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT session_id, seq, run_id, parent_id, agent, kind, step, payload
		FROM session_logs WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: query session logs %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []models.SessionEvent
	for rows.Next() {
		var e models.SessionEvent
		var payload []byte
		if err := rows.Scan(&e.SessionID, &e.Seq, &e.RunID, &e.ParentID, &e.Agent,
			&e.Kind, &e.Step, &payload); err != nil {
			return nil, fmt.Errorf("store: scan session event: %w", err)
		}
		e.Payload = map[string]any{}
		_ = json.Unmarshal(payload, &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// jsonQuote renders s as a JSON string body for the marshal-failure marker.
func jsonQuote(s string) string {
	buf, err := json.Marshal(s)
	if err != nil {
		return `"unencodable marshal error"`
	}
	return string(buf[1 : len(buf)-1])
}
