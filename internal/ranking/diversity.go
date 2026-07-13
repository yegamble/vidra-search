package ranking

import (
	"hash/fnv"
	"sort"
)

// Diversity + exploration primitives for the advanced recommenders (§1.8): MMR
// re-ranking for topical diversity and a deterministic ε-greedy exploration slot.
// Both are pure and seed-deterministic so a feed is reproducible for a given
// session/user (no global math/rand).

// MMRDoc is one candidate for MMR selection: an id, its relevance, and the token
// set (tags + category) used for the topical-similarity penalty.
type MMRDoc struct {
	VideoID   string
	Relevance float64
	Tokens    map[string]struct{}
}

// jaccard is the topical similarity between two token sets: |A∩B| / |A∪B|. Two
// empty sets are treated as dissimilar (0) so tag-less videos are not all forced
// apart or together.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// MMR greedily selects up to k ids maximising the Maximal Marginal Relevance
// objective λ·rel − (1−λ)·maxSim(selected) (Carbonell & Goldstein 1998). λ=1 is
// pure relevance; lower λ trades relevance for topical spread. Deterministic:
// the first pick is the most relevant (id tie-break), and every subsequent pick
// maximises the marginal score with an id tie-break.
func MMR(docs []MMRDoc, lambda float64, k int) []string {
	n := len(docs)
	if n == 0 || k <= 0 {
		return nil
	}
	if k > n {
		k = n
	}

	// Stable candidate order (relevance desc, id asc) so ties are deterministic.
	cand := make([]MMRDoc, n)
	copy(cand, docs)
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].Relevance != cand[j].Relevance {
			return cand[i].Relevance > cand[j].Relevance
		}
		return cand[i].VideoID < cand[j].VideoID
	})

	selected := make([]MMRDoc, 0, k)
	used := make([]bool, n)
	out := make([]string, 0, k)

	for len(out) < k {
		bestIdx := -1
		var bestScore float64
		for i := range cand {
			if used[i] {
				continue
			}
			var maxSim float64
			for _, s := range selected {
				if sim := jaccard(cand[i].Tokens, s.Tokens); sim > maxSim {
					maxSim = sim
				}
			}
			mmr := lambda*cand[i].Relevance - (1-lambda)*maxSim
			// cand is pre-sorted by (relevance desc, id asc); a strict > keeps the
			// first (highest-relevance, lowest-id) candidate on ties → deterministic.
			if bestIdx == -1 || mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			break
		}
		used[bestIdx] = true
		selected = append(selected, cand[bestIdx])
		out = append(out, cand[bestIdx].VideoID)
	}
	return out
}

// hashSeed maps an arbitrary seed string to a stable 64-bit value (FNV-1a). Used
// for exploration so a given session/user always gets the same decision.
func hashSeed(seed string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return h.Sum64()
}

// ExplorationSlot deterministically decides the ε-greedy exploration outcome for
// a feed. With probability epsilon (derived from the seed hash, NOT a global RNG)
// it fires, and returns the index into a fresh-low-view pool of size poolSize to
// surface. fire is false (index 0) when it does not fire or the pool is empty.
// The same seed always yields the same decision, so the feed is reproducible and
// the behaviour is unit-testable with fixed seeds.
func ExplorationSlot(seed string, epsilon float64, poolSize int) (fire bool, index int) {
	if poolSize <= 0 || epsilon <= 0 {
		return false, 0
	}
	h := hashSeed(seed)
	// Low 14 bits → a uniform draw in [0,1) for the ε test.
	draw := float64(h&0x3FFF) / float64(0x4000)
	if draw >= epsilon {
		return false, 0
	}
	// A disjoint slice of the hash chooses which pool item to surface.
	return true, int((h >> 20) % uint64(poolSize))
}
