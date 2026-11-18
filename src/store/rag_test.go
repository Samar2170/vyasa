package store

import (
	"testing"

	"vyasa/src/models"
)

func chunk(id string) models.RAGChunk { return models.RAGChunk{BaseModel: models.BaseModel{ID: id}} }

func TestFuseRRF(t *testing.T) {
	vec := []models.RAGChunk{chunk("a"), chunk("b")}
	lex := []models.RAGChunk{chunk("b"), chunk("c")}

	hits := FuseRRF(3, vec, lex)
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	// b appears on both lists, so it must outrank a and c.
	if hits[0].ID != "b" {
		t.Errorf("top hit = %s, want b", hits[0].ID)
	}
	if hits[1].ID != "a" || hits[2].ID != "c" {
		t.Errorf("tail order = %s,%s, want a,c", hits[1].ID, hits[2].ID)
	}
	if hits[0].Score <= hits[1].Score || hits[1].Score <= hits[2].Score {
		t.Errorf("scores not descending: %v", hits)
	}
}

func TestFuseRRFTiesAreDeterministic(t *testing.T) {
	// a and c each appear once, at the same rank of different lists: both
	// score exactly 1/(rrfK+2), so the tail order is decided by the tie-break.
	vec := []models.RAGChunk{chunk("z"), chunk("a")}
	lex := []models.RAGChunk{chunk("y"), chunk("c")}

	first := FuseRRF(4, vec, lex)
	for i := 0; i < 200; i++ {
		again := FuseRRF(4, vec, lex)
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("tie order unstable across runs: run %d pos %d gave %s, want %s",
					i, j, again[j].ID, first[j].ID)
			}
		}
	}
	if first[2].ID != "a" || first[3].ID != "c" {
		t.Errorf("tail = %s,%s, want a,c (id order)", first[2].ID, first[3].ID)
	}
}

func TestFuseRRFTruncates(t *testing.T) {
	var vec []models.RAGChunk
	for i := 0; i < 10; i++ {
		vec = append(vec, chunk(string(rune('a'+i))))
	}
	hits := FuseRRF(3, vec, nil)
	if len(hits) != 3 || hits[0].ID != "a" {
		t.Errorf("unexpected hits: %+v", hits)
	}
}

func tableChunk(id string) models.RAGChunk {
	c := chunk(id)
	c.Kind = models.HitKindTable
	return c
}

func TestFuseRRFTablesCompeteWithProse(t *testing.T) {
	// a table found by both table lists outranks prose found once; the kind
	// must survive the fusion
	proseVec := []models.RAGChunk{chunk("p1"), chunk("p2")}
	tableVec := []models.RAGChunk{tableChunk("t1")}
	tableLex := []models.RAGChunk{tableChunk("t1")}

	hits := FuseRRF(4, proseVec, tableVec, tableLex)
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(hits))
	}
	if hits[0].ID != "t1" {
		t.Errorf("top hit = %s, want t1 (on two lists)", hits[0].ID)
	}
	if hits[0].Kind != models.HitKindTable {
		t.Errorf("kind lost in fusion: %q", hits[0].Kind)
	}
	if hits[1].Kind == models.HitKindTable {
		t.Errorf("prose hit carries table kind")
	}
}
