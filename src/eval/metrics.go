// Package eval measures RAG retrieval quality against a curated golden
// dataset. Items are questions keyed to the chunks that answer them
// ((document_url, ordinal) pairs, stable across re-indexing); the runner
// replays each question through the production hybrid search and scores
// Hit@k, Recall@k, MRR and nDCG@k.
package eval

import "math"

// Metrics holds the retrieval scores computed over one ranked result list.
// Recall is undefined when the dataset item marks more relevant chunks than
// the result list can ever hold, so it degrades gracefully; MRR is computed
// over the whole ranking regardless of k.
type Metrics struct {
	Hit    float64 // 1 when a relevant chunk appears in the top k
	Recall float64 // fraction of relevant chunks present in the top k
	MRR    float64 // reciprocal rank of the first relevant chunk
	NDCG   float64 // normalized discounted cumulative gain (binary gains)
}

// Score computes the metrics for one ranked relevance vector: rel[i] says
// whether the chunk at rank i (0-based) is relevant. numRelevant is the size
// of the full relevant set, which may exceed len(rel) when relevant chunks
// sit beyond the retrieved window.
func Score(rel []bool, numRelevant, k int) Metrics {
	if numRelevant <= 0 || k <= 0 {
		return Metrics{}
	}
	m := Metrics{}
	topK := k
	if topK > len(rel) {
		topK = len(rel)
	}
	found := 0
	dcg := 0.0
	for i := 0; i < len(rel); i++ {
		if !rel[i] {
			continue
		}
		if m.MRR == 0 {
			m.MRR = 1.0 / float64(i+1)
		}
		if i < topK {
			found++
			dcg += 1.0 / math.Log2(float64(i+2)) // rank i (0-based) -> discount log2(rank+1)
		}
	}
	if found > 0 {
		m.Hit = 1
	}
	// Neighbour tolerance can mark more retrieved chunks relevant than the
	// item has targets; recall never exceeds 1.
	if found > numRelevant {
		found = numRelevant
	}
	m.Recall = float64(found) / float64(numRelevant)
	m.NDCG = dcg / idealDCG(min(numRelevant, k))
	// Neighbour tolerance can credit more chunks than the item has targets,
	// pushing raw DCG past the ideal; clamp to keep nDCG a true ratio.
	if m.NDCG > 1 {
		m.NDCG = 1
	}
	return m
}

// idealDCG is the binary-gain DCG of a perfect ranking of n relevant chunks.
func idealDCG(n int) float64 {
	if n <= 0 {
		return 1 // unreachable: callers guard numRelevant > 0 and k > 0
	}
	dcg := 0.0
	for i := 0; i < n; i++ {
		dcg += 1.0 / math.Log2(float64(i+2))
	}
	return dcg
}
