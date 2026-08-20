// Package migrations embeds the vidra-search SQL migration files into the
// service binary so the published image is self-sufficient: the binary's own
// `migrate up` subcommand (`docker run <image> migrate up`, or `go run ./cmd/api
// migrate up` from a checkout) needs no golang-migrate CLI, no separate migrate
// image, and no bind-mounted git checkout of this directory.
//
// The //go:embed directive has to live HERE: embed can only reach files in the
// directive's own directory (and below), never a parent or sibling one.
package migrations

import "embed"

// FS holds every migration file in this directory — both .up.sql and .down.sql —
// under the exact on-disk names (e.g. "0001_init_schema.up.sql"), which is the
// naming golang-migrate's iofs source driver parses.
//
//go:embed *.sql
var FS embed.FS
