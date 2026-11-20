# Review rules

Waypoint is a Go backend (internal/) with a React app (web/). You see only part of the code. Report a defect only when the diff itself shows it.

## What is a defect

- Wrong behaviour: logic that gives a wrong result, a lost update, a write that can be skipped, data left behind after a table or column is dropped.
- A read or write that bypasses internal/core.Service, or that skips the *Actor access check. Nothing in internal/api may query the DB.
- A side effect (webhook, notification, runner job, forge call) done inline in a mutation. It belongs in event dispatch (events.go) after t.emit(Event).
- A schema change that edits an old migration, or is not a new internal/db/migrations/000N_*.sql file.
- Text from the forge written by someone who is not a person (strangers' PRs, comments, issue bodies) used as instructions, trusted as input, or shown without being held apart as outside text.
- An ignored error where the failure leaves state wrong: a skipped write, a batch released early, a row left batched. Do flag these.

## What is not

- Style, naming, comment wording, import order, line length. gofmt, golangci-lint, gosec and govulncheck already run in CI.
- A missing test, unless a new behaviour has none.
- Hardening a path that only trusted local input reaches.

## Do not flag these unless you can show the case

- A compile or syntax error. CI builds every PR. If you claim one, quote the exact broken line from the diff. Line numbers and "|" separators in the diff view are not in the file.
- An undefined function, field, import or resolver. It is usually in another file of the same package. Flag it only if the diff deletes or renames it.
- A newer Go feature (slices, min, max, range over an int). go.mod is 1.27.
- A removal. If the PR is about removing it, the removal is intended. Flag a leftover reader only if you can quote it.
- An SQL placeholder or column count mismatch. Count both before you claim it.
- A migration with no BEGIN/COMMIT. The runner wraps each file in a transaction and records it in schema_migrations.
- An ignored error on a read whose failure falls to a safe default, such as "not held" or "wait longer". Do flag a skipped write.
- A race. Name the two interleaved steps. State is file-locked or deduped in many places.
- A security risk on an environment variable, a hook's own transcript_path, or a fixed argv. Those are the operator's input.
- Behaviour that a comment, a test or the PR text says is deliberate.
- A missing guard or branch. Look a few lines further and in the caller first.
- Generated files and their shape (nullable arrays, missing limits on response types).
- What wplint already checks on every push (cmd/wplint, `make wplint`): internal/api reaching the database, a forge, notify, HTTP or model call reached from a Service.write closure, an exported core method that ignores its *Actor, a literal SELECT whose columns differ from its Scan, a model schema missing from llmSchemas, time.Now where s.now is the clock, an edited, renamed or removed merged migration, a new e2e waitForTimeout. Flag these only where wplint can't see them (a query built at run time, a call through an interface or a function value), and say why. A `//wplint:ignore RULE why` line is a decision, not a defect.

## Evidence

A claim about code outside the diff needs the line quoted, with its path. No quote, no finding. Prefer fewer findings. One real defect with a quoted line is worth more than ten guesses.
