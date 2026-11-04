//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/vidra/vidra-search/internal/suggest"
)

func TestCatalogSuggestionCacheSeparatesGenerationAndEpoch(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	v := video("catalog old")
	ingest(t, env, upsertEnvelope(t, v))
	req := suggest.Request{Query: "ca", CatalogFence: "epoch-a:1"}
	if got := env.sugg.Suggest(ctx, req); len(got.Suggestions) == 0 {
		t.Fatal("did not populate document cache")
	}
	v.Title = "zzzzzzzzzz"
	ingest(t, env, upsertEnvelope(t, v))
	if got := env.sugg.Suggest(ctx, req); len(got.Suggestions) == 0 {
		t.Fatal("old fence cache was not exercised")
	}
	req.CatalogFence = "epoch-a:2"
	if got := env.sugg.Suggest(ctx, req); len(got.Suggestions) != 0 {
		t.Fatalf("new generation reused old document cache: %+v", got.Suggestions)
	}
	v.Title = "catalog fresh"
	ingest(t, env, upsertEnvelope(t, v))
	req.CatalogFence = "epoch-b:1"
	got := env.sugg.Suggest(ctx, req)
	if len(got.Suggestions) != 1 || got.Suggestions[0].Text != v.Title {
		t.Fatalf("new epoch reused old document cache: %+v", got.Suggestions)
	}
}
