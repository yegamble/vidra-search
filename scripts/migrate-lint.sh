#!/usr/bin/env bash
#
# migrate-lint — forward migrations must be additive.
#
# The one-release schema-compat policy says release N-1's code has to keep
# running against release N's schema; that property is what makes a rollback a
# tag flip instead of a database restore. Any DDL that removes or narrows an
# object the previous release still reads or writes breaks it, so *.up.sql may
# not carry destructive DDL. Down migrations are exempt — they ARE the rollback
# path, and are destructive by definition.
#
# Kept in sync by hand with vidra-core/scripts/migrate-lint.sh: the two copies
# are deliberately identical and carry no repo-specific strings. Change one,
# change the other.
#
# Usage: scripts/migrate-lint.sh [migrations-dir]   (default: <repo>/migrations)
#
# Escape hatch: a line carrying `-- migrate-lint:allow` is skipped, for a compat
# break the team has explicitly accepted (mirrors the `# ci-guard:allow`
# convention in .github/workflows/ci-guard.yml).

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
dir=${1:-$repo_root/migrations}

shopt -s nullglob
files=("$dir"/*.up.sql)
shopt -u nullglob

# A lint that passes because it found nothing to lint is a hole, not a pass.
if [ ${#files[@]} -eq 0 ]; then
  echo "::error::migrate-lint: no *.up.sql files under $dir" >&2
  exit 1
fi

# Matching runs on a normalised copy of each statement: uppercased, with every
# character that cannot appear in an identifier turned into a space and runs of
# spaces collapsed. Every keyword is then a space-delimited token, so a pattern
# padded with spaces is an exact word match and `rename_count` cannot trip the
# RENAME rule. Statements are accumulated across lines up to the terminating
# semicolon so a rule can span the lines of one statement, and a violation is
# reported once, on the line that completed the match.
read -r -d '' lint_awk <<'AWK' || true
function norm(s,   t) {
    t = toupper(s)
    gsub(/[^[:alnum:]_]/, " ", t)
    gsub(/ +/, " ", t)
    return " " t " "
}

# First identifier token in s, skipping the optional IF EXISTS.
function first_token(s,   p, tok) {
    while (s != "") {
        p = index(s, " ")
        if (p == 0) { tok = s; s = "" } else { tok = substr(s, 1, p - 1); s = substr(s, p + 1) }
        if (tok == "" || tok == "IF" || tok == "EXISTS") continue
        return tok
    }
    return ""
}

# Names introduced by `ADD CONSTRAINT <name>` anywhere in the file.
function collect_added(u,   rest, tok) {
    rest = u
    while (match(rest, " ADD CONSTRAINT ")) {
        rest = substr(rest, RSTART + RLENGTH)
        tok = first_token(rest)
        if (tok != "") added[tok] = 1
    }
}

# DROP CONSTRAINT is judged at END: legal only as the widen-a-CHECK idiom, where
# the same file re-adds the same constraint name with a wider predicate.
function scan_drop_constraint(u, lineno,   rest, cname) {
    rest = u
    while (match(rest, " DROP CONSTRAINT ")) {
        rest = substr(rest, RSTART + RLENGTH)
        cname = first_token(rest)
        record(lineno, "DROP CONSTRAINT " (cname == "" ? "<unparsed>" : cname) \
            " with no matching ADD CONSTRAINT of the same name in this file — only the widen-a-CHECK idiom (drop, then re-add the same name) is allowed", cname)
    }
}

# need != "" makes the violation conditional on that constraint name never being
# re-added; the violation list stays in line order either way.
function record(lineno, msg, need) {
    nv++
    v_line[nv] = lineno
    v_msg[nv] = msg
    v_need[nv] = need
}

BEGIN {
    nv = 0
    stmt = ""
    nr = 0
    nr++; pat[nr] = " DROP TABLE ";  rule[nr] = "DROP TABLE"
    nr++; pat[nr] = " DROP COLUMN "; rule[nr] = "DROP COLUMN"
    nr++; pat[nr] = " RENAME ";      rule[nr] = "RENAME"
    nr++; pat[nr] = " TRUNCATE ";    rule[nr] = "TRUNCATE"
    nr++; pat[nr] = " SET NOT NULL "; rule[nr] = "SET NOT NULL"
    nr++; pat[nr] = " ALTER( COLUMN)? [[:alnum:]_]+( SET DATA)? TYPE "; rule[nr] = "ALTER COLUMN ... TYPE"
    nr++; pat[nr] = " DELETE FROM ";  rule[nr] = "DELETE FROM"
    nr++; pat[nr] = " DROP (TYPE|MATERIALIZED VIEW|VIEW|FUNCTION|TRIGGER|SEQUENCE|SCHEMA) "; rule[nr] = "DROP of a schema object"
}

{
    # The escape-hatch marker lives in a comment, so it has to be read before
    # comments are stripped. An allowed line keeps only its semicolons, so the
    # statement boundary survives while the content it excuses does not.
    line = $0
    if (index($0, allow) > 0) {
        gsub(/[^;]/, "", line)
    } else {
        sub(/--.*$/, "", line)             # line comments
        gsub(q "[^" q "]*" q, " ", line)   # string literals ('rename' is not a RENAME)
    }

    u = norm(line)
    collect_added(u)
    scan_drop_constraint(u, FNR)

    parts = split(line, part, ";")
    for (i = 1; i <= parts; i++) {
        prev = norm(stmt)
        stmt = stmt " " part[i]
        cur = norm(stmt)
        for (r = 1; r <= nr; r++)
            if (cur ~ pat[r] && prev !~ pat[r])
                record(FNR, rule[r] " in a forward migration breaks one-release schema compat (the previous release still runs against this schema)", "")
        if (i < parts) stmt = ""           # the semicolon ended the statement
    }
}

END {
    bad = 0
    for (i = 1; i <= nv; i++) {
        if (v_need[i] != "" && (v_need[i] in added)) continue
        printf "::error file=%s,line=%d::%s\n", file, v_line[i], v_msg[i] > "/dev/stderr"
        bad = 1
    }
    if (bad) exit 1
}
AWK

status=0
for f in "${files[@]}"; do
  awk -v file="$f" -v q="'" -v allow='migrate-lint:allow' "$lint_awk" "$f" || status=1
done

if [ "$status" -ne 0 ]; then
  echo "migrate-lint: up migrations must be additive — add tables/columns/indexes, widen CHECKs, relax NOT NULL. Rewrite the step, or mark the line '-- migrate-lint:allow' once the team has accepted the compat break." >&2
  exit 1
fi

echo "✓ migrate-lint: ${#files[@]} up migrations clean (no destructive DDL)."
