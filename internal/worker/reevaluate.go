package worker

import (
	"context"
	"time"

	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// reevalBatchSize bounds one UPDATE, matching the 10k batches the prune queries
// use. reevalMaxBatches caps a single pass at 1M rows so a pathological table
// cannot hold the job open past JobTimeout; the remainder is taken next tick.
// reevalSampleSize is how many changed keys the report and log line name.
const (
	reevalBatchSize  = 10000
	reevalMaxBatches = 100
	reevalSampleSize = 10
)

// ReevalChange is one aggregate row the pass moved, or — in a dry run — would
// move. Suggestible is the value the row takes, not the value it had.
type ReevalChange struct {
	NormalizedQuery string `json:"normalized_query"`
	Suggestible     bool   `json:"suggestible"`
	DistinctUsers   int32  `json:"distinct_users"`
}

// ReevalReport summarises one pass. Changed is exact in both modes: applying
// sums the batches' :execrows, the dry run pages the preview to the end.
type ReevalReport struct {
	DryRun  bool           `json:"dry_run"`
	Skipped bool           `json:"skipped"`
	Changed int64          `json:"changed"`
	Sample  []ReevalChange `json:"sample"`
}

// ReevaluateSuggestible recomputes query_aggregates.suggestible for every row
// against the currently-surviving query_log — see queries/reevaluation.sql for
// why the rollup alone cannot. Exported so integration tests and the one-shot
// SEARCH_RUN_JOB path can observe counts a ticker loop would only log.
//
// dryRun reports what would move and changes nothing. It is the intended first
// step on an instance that has been accumulating unsupported suggestions: an
// operator who cannot see the blast radius before taking it can turn this repair
// into a silent emptying of autosuggest.
func (r *Runner) ReevaluateSuggestible(ctx context.Context, dryRun bool) (ReevalReport, error) {
	ov := r.overlay(ctx)
	minUsers := int32(overlayInt(ov, "minimum_query_user_count", r.cfg.MinQueryUserCount))
	retentionDays := overlayInt(ov, "search_event_retention_days", r.cfg.RetentionDays)
	windowStart := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	q := r.store.Queries()
	rep := ReevalReport{DryRun: dryRun}

	// Refuse to act on an empty ledger. Every aggregate would recount to zero and
	// the whole instance would lose autosuggest in one sweep — and an empty window
	// signals a wiped or not-yet-ingested query_log far more often than it signals
	// a platform nobody has ever searched. This repo has already been burned by a
	// repair pass running on incomplete data; that is the failure being refused.
	hasRows, err := q.QueryLogHasRowsInWindow(ctx, windowStart)
	if err != nil {
		return rep, err
	}
	if !hasRows {
		rep.Skipped = true
		r.logger.WarnContext(ctx, "suggestible_reeval: skipped — no query_log rows in the retention window; refusing to un-suggest every aggregate on an empty ledger",
			"window_start", windowStart, "retention_days", retentionDays)
		return rep, nil
	}

	preview := func(after string, lim int32) ([]sqlcgen.PreviewSuggestibleReevaluationRow, error) {
		return q.PreviewSuggestibleReevaluation(ctx, sqlcgen.PreviewSuggestibleReevaluationParams{
			WindowStart: windowStart, MinUsers: minUsers, After: after, Lim: lim,
		})
	}
	keep := func(rows []sqlcgen.PreviewSuggestibleReevaluationRow) {
		for _, row := range rows {
			if len(rep.Sample) >= reevalSampleSize {
				return
			}
			rep.Sample = append(rep.Sample, ReevalChange{
				NormalizedQuery: row.NormalizedQuery, Suggestible: row.Suggestible, DistinctUsers: row.DistinctUsers,
			})
		}
	}

	if dryRun {
		// Keyset-page the whole changed set so "would change N" is exact rather
		// than "at least one batch's worth".
		for after, batch := "", 0; batch < reevalMaxBatches; batch++ {
			rows, err := preview(after, reevalBatchSize)
			if err != nil {
				return rep, err
			}
			keep(rows)
			rep.Changed += int64(len(rows))
			if len(rows) < reevalBatchSize {
				break
			}
			after = rows[len(rows)-1].NormalizedQuery
		}
		r.logger.InfoContext(ctx, "suggestible_reeval: dry run — no rows changed",
			"would_change", rep.Changed, "min_users", minUsers, "retention_days", retentionDays, "sample", rep.Sample)
		return rep, nil
	}

	// Name a few of the rows about to move, before they move, so the unattended
	// ticker leaves an operator something to recognise in the log.
	rows, err := preview("", reevalSampleSize)
	if err != nil {
		return rep, err
	}
	keep(rows)

	// Each batch touches only rows whose flag actually moves, so the remaining
	// work strictly shrinks and the loop terminates without needing a cursor.
	for batch := 0; batch < reevalMaxBatches; batch++ {
		n, err := q.ReevaluateSuggestible(ctx, sqlcgen.ReevaluateSuggestibleParams{
			WindowStart: windowStart, MinUsers: minUsers, Lim: reevalBatchSize,
		})
		if err != nil {
			return rep, err
		}
		rep.Changed += n
		if n < reevalBatchSize {
			break
		}
	}
	if rep.Changed > 0 {
		r.logger.InfoContext(ctx, "suggestible_reeval: recomputed suggestibility from surviving query_log",
			"changed", rep.Changed, "min_users", minUsers, "retention_days", retentionDays, "sample", rep.Sample)
	}
	return rep, nil
}

// suggestibleReeval is the ticker / one-shot entry point. SEARCH_REEVAL_DRY_RUN
// makes the loop report-only without disabling it, so an operator can watch what
// the pass would do to their data before letting it write.
func (r *Runner) suggestibleReeval(ctx context.Context) error {
	_, err := r.ReevaluateSuggestible(ctx, r.cfg.ReevalDryRun)
	return err
}
