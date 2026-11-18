// Package session persists agent event streams to the session_logs table
// and renders them on the terminal. One CLI invocation that runs agents is
// one session: every event the agents emit (run boundaries, steps, tool
// calls + outputs, streamed text, usage) is stored as a flat event log —
// one jsonb row per event (see src/agent/DESIGN.md §5) — so runs can be
// replayed, inspected and debugged after the fact.
//
// Persistence is deliberately passive: the logger never blocks the agent
// loop and never aborts the workflow on a write failure; it degrades to a
// warning.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vyasa/src/agent"
	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	flushAt       = 50 // events buffered before a batch insert
	flushInterval = 750 * time.Millisecond
	insertTimeout = 15 * time.Second // per-batch write deadline
	chanBuffer    = 512              // events queued before the sink starts dropping
)

// eventStore is the persistence surface (satisfied by *store.Store, and by
// test fakes).
type eventStore interface {
	InsertSessionEvents(ctx context.Context, events []models.SessionEvent) error
}

// Logger persists one session's agent events to Postgres. Events arrive
// from concurrent agent runs through Sink; a single writer goroutine
// batches them (coalescing streamed text deltas per run+step into one row)
// and flushes to the store.
type Logger struct {
	store     eventStore
	sessionID string
	ch        chan agent.Event
	done      chan struct{}
	seq       atomic.Int64
	dropped   atomic.Int64
	closed    atomic.Bool
	endOnce   sync.Once
	sendMu    sync.Mutex

	// writer-goroutine state, only touched by writeLoop and Close (after
	// the writer has stopped).
	textKey  runStep
	textOpen bool
	text     strings.Builder
	warned   atomic.Bool
}

// runStep identifies whose text is being coalesced.
type runStep struct {
	run   string
	agent string
	step  int
}

// New starts a session with the given metadata (command, symbol, model, …)
// persisted as its first row. The returned logger is safe for concurrent
// use; call Close once the workflow finishes to flush and write the
// session_end row.
func New(ctx context.Context, st *store.Store, meta map[string]any) *Logger {
	return newLogger(ctx, st, meta)
}

func newLogger(ctx context.Context, st eventStore, meta map[string]any) *Logger {
	l := &Logger{
		store:     st,
		sessionID: newSessionID(),
		ch:        make(chan agent.Event, chanBuffer),
		done:      make(chan struct{}),
	}
	l.insert(ctx, models.SessionEvent{
		SessionID: l.sessionID,
		Seq:       0,
		Kind:      "session_start",
		Payload:   meta,
	})
	go l.writeLoop()
	return l
}

// Sink returns the agent.EventSink every agent run of the session feeds.
func (l *Logger) Sink() agent.EventSink { return l.send }

// ID returns the session id grouping this session's rows.
func (l *Logger) ID() string { return l.sessionID }

func (l *Logger) send(e agent.Event) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	if l.closed.Load() {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.dropped.Add(1) // never block the agent loop
	}
}

// Close flushes pending events and writes the session_end row with the
// caller's summary (status, usage, cost, warnings, …). Safe to call once;
// later calls are no-ops.
func (l *Logger) Close(summary map[string]any) {
	l.endOnce.Do(func() {
		l.closed.Store(true)
		l.sendMu.Lock()
		close(l.ch)
		l.sendMu.Unlock()
		<-l.done
		l.insert(context.Background(), models.SessionEvent{
			SessionID: l.sessionID,
			Seq:       int(l.seq.Add(1)),
			Kind:      "session_end",
			Payload:   summary,
		})
	})
}

func (l *Logger) writeLoop() {
	defer close(l.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pending := make([]models.SessionEvent, 0, flushAt)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		l.insert(ctx, pending...)
		pending = pending[:0]
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case e, ok := <-l.ch:
			if !ok {
				l.closeText(&pending)
				flush()
				if d := l.dropped.Load(); d > 0 {
					log.Printf("[session] %s: %d events dropped (buffer full)", l.sessionID, d)
				}
				return
			}
			l.append(&pending, e)
			if len(pending) >= flushAt {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// append converts one event into pending rows. Consecutive text_delta
// events of the same run+step are coalesced into a single text row.
func (l *Logger) append(pending *[]models.SessionEvent, e agent.Event) {
	if e.Kind == agent.EventTextDelta {
		key := runStep{run: e.RunID, agent: e.Agent, step: e.Step}
		if !l.textOpen || l.textKey != key {
			l.closeText(pending)
			l.textKey = key
			l.textOpen = true
		}
		l.text.WriteString(e.Text)
		return
	}
	l.closeText(pending)
	if e.Kind == agent.EventLLMStart {
		// Adds nothing beyond the step_start emitted immediately before it.
		return
	}
	*pending = append(*pending, models.SessionEvent{
		SessionID: l.sessionID,
		Seq:       int(l.seq.Add(1)),
		RunID:     e.RunID,
		ParentID:  e.ParentID,
		Agent:     e.Agent,
		Kind:      string(e.Kind),
		Step:      e.Step,
		Payload:   eventPayload(e),
	})
}

func (l *Logger) closeText(pending *[]models.SessionEvent) {
	if !l.textOpen {
		return
	}
	l.textOpen = false
	text := l.text.String()
	l.text.Reset()
	*pending = append(*pending, models.SessionEvent{
		SessionID: l.sessionID,
		Seq:       int(l.seq.Add(1)),
		RunID:     l.textKey.run,
		Agent:     l.textKey.agent,
		Kind:      "text",
		Step:      l.textKey.step,
		Payload:   map[string]any{"text": truncateText(text, maxPayloadText)},
	})
}

func (l *Logger) insert(ctx context.Context, rows ...models.SessionEvent) {
	ictx, cancel := context.WithTimeout(ctx, insertTimeout)
	defer cancel()
	if err := l.store.InsertSessionEvents(ictx, rows); err != nil {
		// Persistence must never break the run: warn once, keep going.
		if l.warned.CompareAndSwap(false, true) {
			log.Printf("[session] %s: persist failed, continuing without logs: %v", l.sessionID, err)
		}
	}
}

// UsageSummary renders an llm.Usage as the jsonb shape stored in payloads.
func UsageSummary(u llm.Usage) map[string]any {
	p := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"cached_tokens": u.CachedTokens,
	}
	if u.Cost != nil {
		p["cost"] = *u.Cost
	}
	return p
}

// Tee fans events out to every sink (session logger + terminal renderer).
func Tee(sinks ...agent.EventSink) agent.EventSink {
	return func(e agent.Event) {
		for _, s := range sinks {
			s(e)
		}
	}
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
