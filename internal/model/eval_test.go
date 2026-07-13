package model

import (
	"math"
	"testing"
)

func TestNDCGPerfectOrderIsOne(t *testing.T) {
	// Labels already in ideal (descending) order → NDCG = 1.
	if got := NDCGAt([]float64{2, 1, 1, 0, 0}, 10); math.Abs(got-1) > 1e-12 {
		t.Fatalf("perfect NDCG = %v, want 1", got)
	}
}

func TestNDCGWorseOrderIsLower(t *testing.T) {
	ideal := NDCGAt([]float64{2, 1, 0}, 10)
	worse := NDCGAt([]float64{0, 1, 2}, 10)
	if worse >= ideal {
		t.Fatalf("reversed order NDCG %v should be < ideal %v", worse, ideal)
	}
	if worse <= 0 {
		t.Fatalf("reversed order still has signal, NDCG should be >0, got %v", worse)
	}
}

func TestNDCGNoPositiveIsZero(t *testing.T) {
	if got := NDCGAt([]float64{0, 0, 0}, 10); got != 0 {
		t.Fatalf("no-positive NDCG = %v, want 0", got)
	}
}

func TestNDCGRespectsCutoff(t *testing.T) {
	// A single relevant item beyond the cutoff contributes nothing at k=1.
	full := NDCGAt([]float64{0, 2}, 2)
	cut := NDCGAt([]float64{0, 2}, 1)
	if cut != 0 {
		t.Fatalf("relevant item beyond k must not count, NDCG@1 = %v", cut)
	}
	if full <= 0 {
		t.Fatalf("NDCG@2 should count the item, got %v", full)
	}
}

func TestMRR(t *testing.T) {
	if got := MRRAt([]float64{0, 0, 1}, 10); math.Abs(got-1.0/3) > 1e-12 {
		t.Fatalf("MRR = %v, want 1/3", got)
	}
	if got := MRRAt([]float64{2, 0}, 10); got != 1.0 {
		t.Fatalf("first-position relevant MRR = %v, want 1", got)
	}
	if got := MRRAt([]float64{0, 0}, 10); got != 0 {
		t.Fatalf("no relevant MRR = %v, want 0", got)
	}
	if got := MRRAt([]float64{0, 0, 1}, 2); got != 0 {
		t.Fatalf("relevant beyond cutoff MRR = %v, want 0", got)
	}
}
