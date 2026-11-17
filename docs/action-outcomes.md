# Durable restart outcomes (Phase 4D1)

## Execution truth

Only `restart_application` is executable. Confirmation authorizes; a separate
Execute dispatches to MiniDeploy exactly once. Refresh and History restoration
are authenticated GETs and perform no mutation or inference.

Action lifecycle states remain unchanged. `executing` means execution started
without a confirmed terminal result; it does not imply that work is still
running. Its projected `execution_outcome` is `outcome_unknown`, including for
legacy interrupted dispatches. Success and failure require their respective
validated audit events. A currently running application is not causal proof.

## Persistence

SQLite schema v4 extends `action_events` to accept
`execution_outcome_unknown` (`executing` to `executing`) with one existing
closed failure category. Existing proposal, History, and event values are
preserved. The migration runs in the existing startup transaction. Schema v3
cannot accept the new event under its CHECK constraints, so a migration is
required rather than overloading a failure event.

Known definitive failures append `execution_failed`. A dispatched request with
an uncertain response appends `execution_outcome_unknown`. Neither can execute
again. A crash or storage failure can leave only `execution_started`; reads
still conservatively report unknown without inventing a failure category or
completion timestamp. GET does not append reconciliation events.

The prior binary supports schema v3 only. After migration, a binary-only
rollback will fail closed. Preserve a consistent database backup before
deployment. Never restore that backup after new activity without separately
preserving and accounting for the new History/action records.

## API and UI

Execute, action GET, and History action GET share the same durable projection:

- `execution_outcome`: `succeeded`, `failed`, or `outcome_unknown` after start;
- `failure_category`: a closed category when recorded;
- `execution_started_at`;
- `execution_completed_at` for confirmed success/failure only;
- `outcome_recorded_at` for an explicit terminal or unknown event.

Administrator responses use `Cache-Control: no-store`. Unknown cards explain
uncertainty and provide GET-only Refresh. There is no retry control or automatic
redispatch. Failure cards render safe category explanations. Existing successful
cards and separate Confirm/Execute controls are retained.

## Verification

Local validation on November 17, 2026:

- `go test -count=1 ./...` passed;
- `go test -race ./...` passed;
- `go vet ./...` passed;
- `node --test frontend_action_controls_test.js frontend_action_app_test.js`
  passed (8 tests);
- JavaScript syntax and `git diff --check` passed.

Tests cover a real local HTTP disconnect after dispatch, restart count, reopen,
Execute/GET/History parity, read-only refresh/reload, completion races, legacy
fixture identities, byte-preserving v3 migration, and actual served frontend
code using a small test DOM adapter.

An earlier-phase defect was corrected: unclassified executor errors can no
longer claim definitive failure. Only typed evidence of non-execution can do so.

## Production acceptance checkpoint

November 17, 2026: implementation commit
`87cd2968fefe5931e257ae6686acde4ccec24927` was deployed after the user completed
sudo authentication. Schema v4 and all pre-deployment historical row hashes
were preserved. Backup and old binary remain under
`/var/lib/miniai/phase4d1-87cd296`.

- Missing-target action `action_34837674e3c5a8c38a90c852484419e3` recorded
  `failed / upstream_rejected`. GET and browser reload preserved it; duplicate
  Execute returned 409 without new events.
- Canary action `action_9bf4f2176b9315bce25d727e2836f953` used the ordinary
  authenticated direct-user policy. Creation did not restart; separate Execute
  succeeded. Execute/GET matched, reload preserved the outcome, and duplicate
  Execute returned 409 without new events. The same canary container was left
  healthy, with start `2026-11-17T19:59:32.047913331Z`.
- The public failure response was gateway HTML, not the origin JSON body. A new
  actual-app frontend regression proves this becomes one GET reconciliation,
  safe failed rendering, and no repeated Execute. All 9 frontend tests pass.
  This follow-up changes only tests/documentation, not the deployed implementation.
- Live ambiguity was intentionally not induced by disrupting production; the
  real local disconnect/reopen/no-retry integration tests provide that proof.
- Four bounded read-only investigations produced no assistant action (two
  validated-answer rejections, one generic truncated answer, one canceled
  stream). Fresh assistant -> Confirm -> Execute live acceptance remains
  unproven. The validator and authentication were not weakened to force a pass.

Status is PARTIAL, not phase completion. Automated confirmation separation and
earlier Phase 4C historical evidence remain intact, but direct-user success is
not substituted for the missing fresh assistant-flow proof. The ReactorLab
`PRODUCTION_VERIFICATION.md` and master `REACTORLAB_CONTEXT.md` record the exact
remaining checks, including the requested final protected-database hash check.
Phase 4D2 is not started.
