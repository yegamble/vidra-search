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

func main() {
	mode := flag.String("mode", "seed", "seed | drive")
	n := flag.Int("n", 100000, "number of synthetic documents to seed")
	base := flag.String("base", envOr("SEARCH_BASE_URL", "http://localhost:8081"), "base URL of the running service (drive mode)")
	rps := flag.Int("rps", 50, "target requests per second (drive mode)")
	duration := flag.Duration("duration", 30*time.Second, "load duration (drive mode)")
	flag.Parse()

	switch *mode {
	case "seed":
		if err := seed(*n); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			os.Exit(1)
		}
	case "drive":
		if err := drive(*base, *rps, *duration); err != nil {
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
		title := fmt.Sprintf("%s %s %s", pick(r, adjectives), pick(r, nouns), pick(r, nouns))
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
func drive(base string, rps int, duration time.Duration) error {
	secret := os.Getenv("INTERNAL_SECRET")
	if secret == "" {
		secret = "dev-insecure-internal-secret-change-me-0000"
	}
	prefixes := buildPrefixes()
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
			d, err := oneRequest(client, base, secret, prefix)
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

func oneRequest(client *http.Client, base, secret, prefix string) (time.Duration, error) {
	path := "/internal/v1/suggestions"
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
