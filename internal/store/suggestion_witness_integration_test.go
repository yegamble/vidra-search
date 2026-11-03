//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/suggest"
)

func TestIntegrationDocumentSuggestionWitnessesSurviveCache(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	makeDoc := func(title, handle string, views int64) event.VideoDoc {
		return video(title, func(v *event.VideoDoc) {
			v.ChannelName, v.ChannelHandle = ptr("witnesschannel"), ptr(handle)
			v.Tags, v.Views = []string{"witnesstag"}, views
		})
	}
	low := makeDoc("first unrelated title", "aaa", 10)
	high := makeDoc("second unrelated title", "zzz", 100)
	private := makeDoc("private unrelated title", "private", 9000)
	private.Privacy = "private"
	sensitive := makeDoc("sensitive unrelated title", "sensitive", 500)
	sensitive.IsSensitive = true
	ingest(t, env, upsertEnvelope(t, low), upsertEnvelope(t, high),
		upsertEnvelope(t, private), upsertEnvelope(t, sensitive))
	request := suggest.Request{Query: "wi", Limit: 20, HideSensitive: true}
	check := func(response suggest.Response) {
		t.Helper()
		found := map[ranking.Kind]bool{}
		for _, item := range response.Suggestions {
			if item.Type != ranking.KindChannel && item.Type != ranking.KindTag {
				continue
			}
			found[item.Type] = true
			if item.VideoID != high.ID.String() {
				t.Errorf("%s witness = %q, want %s", item.Type, item.VideoID, high.ID)
			}
			if item.Type == ranking.KindChannel && item.ChannelHandle != "zzz" {
				t.Errorf("channel handle %q did not come from its representative row", item.ChannelHandle)
			}
		}
		if !found[ranking.KindChannel] || !found[ranking.KindTag] {
			t.Fatalf("missing channel/tag suggestions: %+v", response.Suggestions)
		}
	}
	check(env.sugg.Suggest(ctx, request))
	// Remove the projection after filling the short-prefix cache. The second
	// response must retain its source identifiers through the real Redis codec.
	if _, err := env.store.Pool.Exec(ctx, `DELETE FROM search.documents`); err != nil {
		t.Fatal(err)
	}
	check(env.sugg.Suggest(ctx, request))
}
