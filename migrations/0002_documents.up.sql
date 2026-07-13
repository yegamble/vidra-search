-- 0002: the search corpus.
--
-- One row per indexable video (local or remote). vidra-core owns the source of
-- truth and pushes changes as domain events (video.upsert/suppress/stats,
-- channel.*, user.suppress, reconcile.*); this table is a denormalized,
-- search-optimized projection of that stream.

-- array_to_string is only declared STABLE (generically, element output
-- functions could be stable), so PostgreSQL refuses it inside a GENERATED
-- column. For a text[] it is genuinely immutable, so we wrap it in an IMMUTABLE
-- SQL function that produces the byte-identical space-joined string. This keeps
-- the tsv definition semantically exactly as specified (tags weighted 'B').
CREATE FUNCTION search.tags_to_text(arr text[]) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN array_to_string(arr, ' ');
--
-- `eligible` is the STATIC visibility gate the index can safely bake in:
-- privacy=public AND state=published AND not blocked/quarantined AND the owner
-- is not unlisted. Per-viewer visibility (mutes/blocks) is NEVER stored here —
-- vidra-core applies it when hydrating the returned ids. `is_sensitive` lets
-- core pass hide_sensitive per request.
CREATE TABLE search.documents (
    video_id          UUID PRIMARY KEY,
    kind              TEXT NOT NULL DEFAULT 'local' CHECK (kind IN ('local', 'remote')),
    channel_id        UUID,
    channel_handle    TEXT,
    channel_name      TEXT,
    owner_id          UUID,
    title             TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    tags              TEXT[] NOT NULL DEFAULT '{}',
    category          TEXT,
    language          TEXT,
    duration_seconds  INT,
    is_sensitive      BOOLEAN NOT NULL DEFAULT false,
    eligible          BOOLEAN NOT NULL DEFAULT false,
    suppressed_reason TEXT,
    views             BIGINT NOT NULL DEFAULT 0,
    likes             INT NOT NULL DEFAULT 0,
    published_at      TIMESTAMPTZ,
    source_updated_at TIMESTAMPTZ NOT NULL,
    indexed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    reconcile_run_id  UUID,
    -- Weighted full-text vector: title (A) > tags/channel (B) > description (C).
    -- 'simple' config (no stemming/stopwords) keeps matching language-agnostic,
    -- which suits a multi-lingual catalog; ranking layers freshness/popularity on
    -- top. Generated+stored so it is always in sync and GIN-indexable.
    tsv tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(search.tags_to_text(tags), '')), 'B') ||
        setweight(to_tsvector('simple', coalesce(channel_name, '')), 'B') ||
        setweight(to_tsvector('simple', left(coalesce(description, ''), 2000)), 'C')
    ) STORED
);

-- Full-text search.
CREATE INDEX documents_tsv_idx ON search.documents USING GIN (tsv);
-- Trigram fuzzy/typo matching on titles (search candidate + suggestion fallback).
CREATE INDEX documents_title_trgm_idx ON search.documents USING GIN (title gin_trgm_ops);
-- The eligible-feed hot path: recent eligible docs (home "fresh", popular fill).
CREATE INDEX documents_eligible_published_idx ON search.documents (eligible, published_at DESC);
-- Channel fan-out (channel.* denormalization, related same-channel).
CREATE INDEX documents_channel_idx ON search.documents (channel_id);
-- Tag membership / overlap.
CREATE INDEX documents_tags_idx ON search.documents USING GIN (tags);
-- Anchored prefix completion for suggestions (LIKE 'p%'): text_pattern_ops makes
-- lower(title)/lower(channel_name) LIKE anchored-prefix queries index-driven.
CREATE INDEX documents_lower_title_prefix_idx ON search.documents (lower(title) text_pattern_ops);
CREATE INDEX documents_lower_channel_name_prefix_idx ON search.documents (lower(channel_name) text_pattern_ops);
