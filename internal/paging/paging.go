// Package paging holds the limit/offset clamp every list endpoint applies to a
// caller-supplied page. It exists because the same clamp had been written four
// times with three different answers for a negative limit — which is a value
// clients do send, and which the api layer forwards verbatim (qInt returns the
// parsed number, defaults are the service's job). One implementation makes the
// public surface answer the same way everywhere.
//
// Each caller keeps its OWN default and maximum: 10/20 suggestions and 20/200
// search hits are product decisions that belong next to the endpoint, not here.
package paging

// Limit returns the page size to use for a caller-supplied limit: def when the
// caller asked for nothing usable (zero, or a negative that cannot mean a page
// size), max when they asked for more than the endpoint will serve.
func Limit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// Offset returns the row offset to use, flooring a negative — which would make
// SQL error rather than page — at the first row.
func Offset(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
