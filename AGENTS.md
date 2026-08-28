# AGENTS.md — vidra-search

Go search service for vidra, a self-hostable video platform: search,
autosuggest, trending and recommendations — ranked IDs in, nothing leaked out.
Fed by vidra-core's search outbox over an HMAC-authenticated internal API.
Conventions mirror vidra-core (Echo, sqlc, golang-migrate); files carrying a
`TWIN` comment must stay byte-identical with their vidra-core counterpart —
fix both repos in the same sweep, never just one.

## Verification gates (run before opening any PR; paste the output tail into the PR body)

```
make ci        # fmt-check, vet, migrate-lint, openapi-verify, sqlc-verify, test-race
```

## Hard rules

1. **One small PR per session** (< 300 changed lines). List every other
   finding in the PR body under "Also found (not fixed here)".
2. **Never hand-edit `internal/store/sqlcgen/**`** — edit the queries under
   `internal/store/queries/` and run `make sqlc`.
3. **Migrations are append-only**: new file with the next number and a
   matching `.down.sql`. Never edit an existing migration.
4. **Do not bump dependencies** (Dependabot owns bumps), do not touch
   `.github/workflows`, never commit secrets or `.env` files.

## Git hygiene — finished means merged (all agents / AI tools)

These rules bind every AI tool working in this repo (Claude, Jules, Codex, …):

1. **Commit early, push often.** Work on a short-lived branch off `main`.
   Prefer several small, scoped commits over one session-end mega-commit, and
   push the branch at every green checkpoint — unpushed work does not exist.
2. **A task is finished only when its work is merged to `main` and pushed.**
   Once the verification gates and the PR's CI are green, merge the PR before
   declaring the task done. If you cannot merge (no permission, review
   requested, red CI), report the task as **open — awaiting merge**, never as
   finished/complete/done.
3. **Delete merged branches.** Immediately after a merge: delete the work
   branch on the remote (`git push origin --delete <branch>`), delete it
   locally (`git branch -d <branch>`), then `git fetch --prune`. Also sweep
   for leftovers each session: delete any local (`git branch --merged
   origin/main`) or remote (`git branch -r --merged origin/main`) branch
   already merged into `origin/main`. Never delete `main`, the branch you are
   on, or an unmerged branch — an unmerged stray is reported for triage, not
   deleted.

## PR conventions

- Title: `[<agent>] <area>: <summary>`.
- Body opens with a one-line WHY, then the verification output tail.
- Never describe an exploitable-but-unfixed security issue in detail in a
  public PR or issue — flag it as "security: needs owner attention" with
  minimal detail.
