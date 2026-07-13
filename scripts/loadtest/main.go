// Command loadtest seeds a synthetic corpus and drives the suggestions endpoint
// to measure p50/p95/p99 latency. It is the W7 performance harness; it depends
// on nothing external (no `hey`/`vegeta`) — the driver is a tiny in-process HTTP
// load generator that signs each request with the internal HMAC header.
//
//	# seed 100k synthetic documents into the DB
//	DATABASE_URL=... go run ./scripts/loadtest -mode=seed -n=100000
//
//	# drive the running service at 50 rps for 30s (needs the api up + secret)
//	INTERNAL_SECRET=... go run ./scripts/loadtest -mode=drive \
//	    -base=http://localhost:8081 -rps=50 -duration=30s
//
//	# drive the search endpoint instead of suggestions
//	INTERNAL_SECRET=... go run ./scripts/loadtest -mode=drive \
//	    -endpoint=search -base=http://localhost:8081 -rps=50 -duration=30s
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vidra/vidra-search/internal/api"
)

var (
	adjectives = []string{"amazing", "advanced", "ancient", "brilliant", "basic", "creative", "curious", "daily", "deep", "epic", "fast", "fresh", "golang", "great", "hidden", "modern", "quick", "rare", "simple", "super", "ultimate", "vivid", "wild", "zen"}
	nouns      = []string{"tutorial", "guide", "review", "journey", "story", "adventure", "recipe", "workout", "concert", "lecture", "demo", "showcase", "experiment", "documentary", "podcast", "highlights", "walkthrough", "analysis", "cooking", "gaming"}
	languages  = []string{"en", "es", "fr", "de", "pt", "ja"}
	categories = []string{"education", "music", "gaming", "sports", "news", "tech"}
)

// numTopics is the size of the synthetic "topic" vocabulary woven into every
// title. A real 100k-video corpus has tens of thousands of distinct title words,
// so a specific search query matches only a small fraction of documents; with
// only the 44 adjective/noun words the corpus would be pathologically dense
// (every term matching ~5–14% of docs), which is not representative of real
// search selectivity. Each title carries one topic word, so a topic query
// matches ≈ n/numTopics documents (index-driven recall, not a full scan).
const numTopics = 8000

// topicWord maps a topic index to a deterministic 7-letter pseudo-word. The
// multiply-by-a-large-prime spreads consecutive indices across the keyspace so
// distinct topics share few trigrams (a shared prefix like "topic00042" would be
// trigram-similar to every other topic and make the `%` recall match the whole
// table — the opposite of a representative, selective query).
func topicWord(i int) string {
	x := uint64(i)*2654435761 + 1099511628211
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 7)
	for j := range b {
		b[j] = alpha[x%26]
		x /= 26
	}
	return string(b)
}

func main() {
	mode := flag.String("mode", "seed", "seed | drive")
	n := flag.Int("n", 100000, "number of synthetic documents to seed")
	base := flag.String("base", envOr("SEARCH_BASE_URL", "http://localhost:8081"), "base URL of the running service (drive mode)")
	rps := flag.Int("rps", 50, "target requests per second (drive mode)")
	duration := flag.Duration("duration", 30*time.Second, "load duration (drive mode)")
	endpoint := flag.String("endpoint", "suggestions", "suggestions | search (drive mode)")
	flag.Parse()

	switch *mode {
	case "seed":
		if err := seed(*n); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			os.Exit(1)
		}
	case "drive":
		path, ok := endpointPath(*endpoint)
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown endpoint:", *endpoint, "(want suggestions | search)")
			os.Exit(2)
		}
		if err := drive(*base, path, buildQueries(*endpoint), *rps, *duration); err != nil {
			fmt.Fprintln(os.Stderr, "drive:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

// seed bulk-loads n synthetic eligible documents via COPY.
func seed(n int) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	r := rand.New(rand.NewSource(42)) // fixed seed → reproducible corpus
	cols := []string{"video_id", "channel_id", "channel_name", "channel_handle", "title", "tags", "category", "language", "eligible", "views", "published_at", "source_updated_at"}
	rows := make([][]any, 0, n)
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("%s %s %s", pick(r, adjectives), topicWord(r.Intn(numTopics)), pick(r, nouns))
		channelID := uuid.New()
		chName := pick(r, adjectives) + " channel"
		tags := []string{pick(r, adjectives), pick(r, nouns)}
		published := now.Add(-time.Duration(r.Intn(365*24)) * time.Hour)
		rows = append(rows, []any{
			uuid.New(), channelID, chName, "ch_" + channelID.String()[:8],
			title, tags, pick(r, categories), pick(r, languages),
			true, int64(zipf(r)), published, published,
		})
	}
	start := time.Now()
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{"search", "documents"}, cols, pgx.CopyFromRows(rows))
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d documents in %s\n", copied, time.Since(start).Round(time.Millisecond))
	return nil
}

// drive fires suggestion requests at the target rate for the duration and prints
// the latency distribution.
// endpointPath maps the -endpoint flag to the internal path driven under load.
func endpointPath(endpoint string) (string, bool) {
	switch endpoint {
	case "suggestions":
		return "/internal/v1/suggestions", true
	case "search":
		return "/internal/v1/search", true
	default:
		return "", false
	}
}

func drive(base, path string, queries []string, rps int, duration time.Duration) error {
	secret := os.Getenv("INTERNAL_SECRET")
	if secret == "" {
		secret = "dev-insecure-internal-secret-change-me-0000"
	}
	prefixes := queries
	client := &http.Client{Timeout: 5 * time.Second}

	var (
		mu       sync.Mutex
		latency  []time.Duration
		errCount int
		okCount  int
	)
	interval := time.Second / time.Duration(max(rps, 1))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for now := range ticker.C {
		if now.After(deadline) {
			break
		}
		prefix := prefixes[r.Intn(len(prefixes))]
		wg.Add(1)
		go func(prefix string) {
			defer wg.Done()
			d, err := oneRequest(client, base, path, secret, prefix)
			mu.Lock()
			if err != nil {
				errCount++
			} else {
				okCount++
				latency = append(latency, d)
			}
			mu.Unlock()
		}(prefix)
	}
	wg.Wait()

	if len(latency) == 0 {
		return fmt.Errorf("no successful requests (errors=%d) — is the service up at %s?", errCount, base)
	}
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	fmt.Printf("requests: ok=%d err=%d\n", okCount, errCount)
	fmt.Printf("p50=%s p95=%s p99=%s max=%s\n",
		pctl(latency, 50).Round(time.Microsecond),
		pctl(latency, 95).Round(time.Microsecond),
		pctl(latency, 99).Round(time.Microsecond),
		latency[len(latency)-1].Round(time.Microsecond))
	return nil
}

func oneRequest(client *http.Client, base, path, secret, prefix string) (time.Duration, error) {
	req, err := http.NewRequest(http.MethodGet, base+path+"?q="+prefix+"&limit=10", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Vidra-Internal-Auth", api.BuildInternalAuthHeader(secret, time.Now().Unix(), http.MethodGet, path))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	return elapsed, nil
}

// buildQueries returns the query set for the driven endpoint: short prefixes for
// autocomplete (suggestions), and full words / two-word phrases for search —
// short 1–2 char prefixes are the autocomplete workload, not the search one (and
// pg_trgm cannot use its index below 3 chars, so they are not representative of
// real search traffic).
func buildQueries(endpoint string) []string {
	if endpoint == "search" {
		return buildSearchTerms()
	}
	return buildPrefixes()
}

// buildSearchTerms builds a representative search workload: specific topic words
// (each matching ≈ n/numTopics documents, like a real user query that resolves to
// a handful of results) mixed with "topic noun" two-word phrases. Full words, all
// >= 3 chars — short 1–2 char prefixes are the autocomplete workload, not search.
func buildSearchTerms() []string {
	out := make([]string, 0, numTopics+len(nouns))
	for i := 0; i < numTopics; i++ {
		out = append(out, topicWord(i))
		if i < len(nouns) { // a spread of two-term phrases exercises AND recall
			out = append(out, topicWord(i)+"%20"+nouns[i])
		}
	}
	return out
}

// buildPrefixes derives a realistic mix of 1–4 character prefixes from the
// seed vocabulary.
func buildPrefixes() []string {
	set := map[string]bool{}
	for _, w := range append(append([]string{}, adjectives...), nouns...) {
		for l := 1; l <= 4 && l <= len(w); l++ {
			set[w[:l]] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func pctl(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func pick(r *rand.Rand, s []string) string { return s[r.Intn(len(s))] }

// zipf returns a rough power-law view count so a few docs are very popular.
func zipf(r *rand.Rand) int {
	return int(float64(1000) / (float64(r.Intn(1000) + 1)) * float64(r.Intn(1000)))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
