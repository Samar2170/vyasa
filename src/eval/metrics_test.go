package eval

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScoreHitAndRecall(t *testing.T) {
	cases := []struct {
		name string
		rel  []bool
		nr   int
		k    int
		hit  float64
		rec  float64
	}{
		{"miss below k", []bool{false, false, true, false}, 1, 2, 0, 0},
		{"hit at cutoff", []bool{false, true, false, false}, 1, 2, 1, 1},
		{"partial recall", []bool{true, false, true, false}, 2, 3, 1, 1},
		{"partial recall two sets", []bool{true, false, true, false}, 3, 3, 1, 2.0 / 3},
		{"relevant beyond window", []bool{false, false, false}, 2, 3, 0, 0},
		{"empty results", nil, 1, 3, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Score(tc.rel, tc.nr, tc.k)
			if m.Hit != tc.hit {
				t.Errorf("Hit = %v, want %v", m.Hit, tc.hit)
			}
			if !near(m.Recall, tc.rec) {
				t.Errorf("Recall = %v, want %v", m.Recall, tc.rec)
			}
		})
	}
}

func TestScoreMRR(t *testing.T) {
	cases := []struct {
		name string
		rel  []bool
		mrr  float64
	}{
		{"first rank", []bool{true, false}, 1},
		{"second rank", []bool{false, true, false}, 0.5},
		{"none", []bool{false, false}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if m := Score(tc.rel, 1, len(tc.rel)); !near(m.MRR, tc.mrr) {
				t.Errorf("MRR = %v, want %v", m.MRR, tc.mrr)
			}
		})
	}
	// MRR ignores k: a relevant chunk at rank 5 counts when k = 3.
	if m := Score([]bool{false, false, false, false, true}, 1, 3); !near(m.MRR, 0.2) {
		t.Errorf("MRR = %v, want 0.2", m.MRR)
	}
}

func TestScoreNDCG(t *testing.T) {
	cases := []struct {
		name string
		rel  []bool
		nr   int
		k    int
		want float64
	}{
		{"perfect", []bool{true, true, false}, 2, 2, 1},
		{"swapped", []bool{false, true, false}, 1, 2, 1 / math.Log2(3)},
		{"third rank", []bool{false, false, true}, 1, 3, 1 / math.Log2(4)},
		{"two of three", []bool{true, false, true}, 3, 3, 1.5 / (1 + 1/math.Log2(3) + 0.5)},
		{"miss", []bool{false, false}, 1, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.rel, tc.nr, tc.k).NDCG
			if !near(got, tc.want) {
				t.Errorf("NDCG = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScoreGuards(t *testing.T) {
	if m := Score([]bool{true}, 0, 3); m != (Metrics{}) {
		t.Errorf("numRelevant = 0 should return zero metrics, got %+v", m)
	}
	if m := Score([]bool{true}, 1, 0); m != (Metrics{}) {
		t.Errorf("k = 0 should return zero metrics, got %+v", m)
	}
}
