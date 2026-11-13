# Split Storage CRUD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the monolithic repository with five entity repositories implementing safe, workspace-scoped basic CRUD.

**Architecture:** Share one PostgreSQL pool. Each repository owns a sqlc query wrapper; domain objects remain driver-independent. Serialize message mutations through the parent thread and invalidate covered summaries transactionally.

**Tech Stack:** Go, pgx/v5, sqlc, goose SQL migrations, local PostgreSQL 17.

**Spec:** `docs/plans/2026-11-10-storage-crud-design.md`

## Global Constraints

- Preserve the current schema and all unrelated user changes; do not commit unless asked.
- No generic CRUD framework, new HTTP API, ingest wiring, credential management, UUID generator, vector search, database selector or applied Supabase migration.
- Domain types stay independent of pgx/sqlc; English comments and documentation.
- Caller-supplied UUIDs; entity ownership and source identity are immutable.
- All child operations are workspace-scoped; no cascade deletes.
- Integration results exclude encrypted_credentials and updates leave it untouched.
- Lists require limit 1..100 and nonnegative offset; stable ordering and empty slices.
- Integration tests use an explicit isolated TEST_DATABASE_URL, never DATABASE_URL.

## Review Focus

1. Cross-workspace reads, writes and parent IDs must not leak or mutate other tenants (Tasks 2–4).
2. Editing/deleting summarized messages must clear both summary fields or rollback entirely (Task 3).
3. Reversed, differently cased UUID link endpoints must identify the same link; self-links must fail (Task 4).
4. JSON encoding failures, nullable source IDs and missing-row errors must not be silently converted to valid data (Tasks 1–4).
5. Foreign-key failures and context cancellation must leave previous data/summary intact and release transactions (Tasks 2–3).

## File Structure and Interface Convention

- `internal/adapters/storage/{workspace,integration,thread,message,thread_link}_repository.go`: concrete types, constructors, CRUD inputs/methods.
- `internal/adapters/storage/mapping.go`: UUID, JSON, pagination and SQL/domain conversion.
- `internal/adapters/storage/*_repository_test.go`, `mapping_test.go`, `testdb_test.go`: behavioral tests and opt-in isolated DB fixture.
- `db/query/{workspace,integration,thread,message,thread_link}.sql`: named queries; regenerate `internal/adapters/storage/sqlc/` with `make generate`.
- Entity-owning packages: add error files for missing Integration/Thread/Message/ThreadLink; retain tenant.ErrWorkspaceNotFound.
- `internal/config/config.go`, `cmd/server/main.go`: fix build prerequisite and remove unused repository initialization.
- Remove `internal/adapters/storage/repository.go` only after its workspace operations have moved and references have been updated.

For each entity E, expose `NewERepository(pool *pgxpool.Pool) (*ERepository, error)`.
Methods use the following exact convention, with fields specified by the spec:

```go
Create(ctx context.Context, input CreateEInput) (domain.E, error)
Get(ctx context.Context, workspaceID, id string) (domain.E, error)
List(ctx context.Context, workspaceID string, limit, offset int32) ([]domain.E, error)
Update(ctx context.Context, input UpdateEInput) (domain.E, error)
Delete(ctx context.Context, workspaceID, id string) error
```

Exceptions: Workspace Get/Delete take only id, List takes only pagination;
Message Get/Delete take workspaceID, threadID string and id int64, List takes
workspaceID, threadID plus pagination; ThreadLink Get/Delete take workspaceID,
sourceThreadID, targetThreadID strings, List takes workspaceID, threadID plus
pagination. Update inputs always include the full scoped key plus only mutable
fields; Message create input includes WorkspaceID for authorization scoping.
ThreadLink create/update return relay.ThreadLink. Use tenant/memory/relay instead
of the schematic domain prefix above; no new domain entity types.

### Task 1: Workspace repository, shared conversions and test fixture

**Files:** workspace query/repository/tests, mapping.go/tests, testdb_test.go, config.go; existing monolithic repository/main.go only as needed to preserve compilation.

**Interfaces:** WorkspaceRepository contract above returning tenant.Workspace; helpers `parseDBID(string) (pgtype.UUID, error)`, `encodeObject(map[string]any) ([]byte, error)` and `validatePage(limit, offset int32) error`.

- [ ] Write mapping tests: reject empty/malformed UUID, accept a valid UUIDv7, encode nil map as `{}`, reject unencodable map values; reject limits 0/101 and offset -1, accept 1/100 and offset 0.
- [ ] Run `go test ./internal/adapters/storage -run 'Test(ParseDBID|EncodeObject|ValidatePage)' -count=1`; verify the new assertions fail before helpers exist.
- [ ] Implement helpers; fix Config.DB type spelling to DatabaseConfig. Add an opt-in test fixture that creates a fresh database on the dedicated local test instance, applies migration Up, and drops only its own database during cleanup. Skip with an explicit message when TEST_DATABASE_URL is absent.
- [ ] Write TestWorkspaceCRUD: create, get, paginated list, rename with refreshed updated_at, delete, then errors.Is(tenant.ErrWorkspaceNotFound) for get/update/delete. Also assert invalid input is rejected before queries and duplicate IDs preserve the database error cause.
- [ ] Run the new tests against isolated PostgreSQL and observe failure before the repository exists.
- [ ] Implement WorkspaceRepository using sqlc named queries, UPDATE/DELETE RETURNING for absent-row detection, stable ordering and mapping. Move old workspace behavior out of Repository without retaining a second persistence implementation.
- [ ] Run workspace/mapping tests and `go test ./...`; report any remaining build blocker by name. Do not contact Supabase.

### Task 2: Integration and thread repositories

**Files:** integration/thread SQL and repository/test files, tenant/errors.go, relay/errors.go, mapping.go.

**Interfaces:** IntegrationRepository returning tenant.Integration; ThreadRepository returning relay.Thread; tenant.ErrIntegrationNotFound and relay.ErrThreadNotFound.

- [ ] Write TestIntegrationCRUD and TestThreadCRUD: round-trip all create fields, replace only mutable fields, preserve source identity/created_at, stable paginated lists, missing-row errors, JSON object mapping and restrictive deletion.
- [ ] Add TestChildWorkspaceIsolation: Get/Update/Delete/List with the wrong workspace cannot expose or mutate a real integration/thread; create with another workspace's parent fails. Test duplicate scoped external thread identity and same external ID in different scopes.
- [ ] Add TestIntegrationCredentialsPreserved: seed ciphertext directly in the isolated database; ordinary integration update must leave it unchanged and never include it in domain results. Test unknown nonblank platforms remain supported.
- [ ] Run new tests and confirm expected failures before adding implementations.
- [ ] Implement scoped queries and repository mappings. Filter UPDATE/DELETE by both workspace and ID; never update ownership/identity. Do not select encrypted_credentials. Retain %w causes on constraint failures.
- [ ] Run the new tests and `go test ./...`.

### Task 3: Message CRUD with transactional summary invalidation

**Files:** message SQL/repository/tests, thread locking/invalidation queries, memory/errors.go, mapping.go.

**Interfaces:** MessageRepository returning memory.Message; memory.ErrMessageNotFound. Transactions use pool.Begin, sqlc.WithTx, Commit and deferred Rollback on all error paths.

- [ ] Write TestMessageCRUD: identity assigned by database, source timestamp preserved, zero timestamp filled, nil sender/source IDs round-trip, multiple local messages allowed, duplicate external ID rejected, immutable author/source identity, ordered pagination and missing-row errors.
- [ ] Write TestMessageWorkspaceIsolation: all operations validate workspace/thread relationship; mismatched message/thread or cross-tenant parents must not mutate real rows.
- [ ] Write TestMessageSummaryInvalidation: seed three messages and a summary through the second; edit/delete first or cursor message clears both summary fields; edit/delete third preserves summary. Deleting the cursor must succeed without leaving a dangling FK. Message writes refresh thread.updated_at.
- [ ] Write TestMessageRollback: induce a failing mutation (including invalid JSON and canceled context); original content/summary survive. Test an insert waiting behind a thread lock observes its context deadline, then a subsequent mutation succeeds without a leaked transaction/lock.
- [ ] Run new tests against isolated PostgreSQL and confirm expected failures before implementation.
- [ ] Implement message writes: begin transaction; SELECT parent FOR UPDATE scoped by workspace before insert/ID allocation; fetch affected row for update/delete; invalidate covered summary before deletion; mutate and refresh thread timestamp; commit. Use a bounded independent cleanup context for rollback when the request is canceled. Keep summary generation outside CRUD.
- [ ] Run message tests with `-race -count=1` and the full Go suite.

### Task 4: Symmetric thread-link CRUD

**Files:** thread_link SQL/repository/tests, relay/errors.go, mapping.go.

**Interfaces:** ThreadLinkRepository returning relay.ThreadLink; relay.ErrThreadLinkNotFound; canonical endpoint conversion compares UUID bytes, not raw input strings.

- [ ] Write TestThreadLinkCRUD: create/get/list/update metadata/delete using the composite key; both endpoint directions and uppercase UUID input address the same link, ordering/pagination are stable, self-link is rejected, duplicate reversed create fails.
- [ ] Add TestThreadLinkWorkspaceIsolation: both endpoints must belong to the requested workspace for create/update; wrong workspace and unrelated endpoint lists cannot reveal a link. Existing links prevent deletion of referenced threads until explicitly unlinked.
- [ ] Run tests and confirm expected failure before implementation.
- [ ] Implement scoped SQL plus canonicalization for every keyed operation; update metadata only, preserving creation time; list uses either endpoint within the workspace.
- [ ] Run link tests and full Go suite.

### Task 5: Remove obsolete setup and verify the whole slice

**Files:** repository.go, main.go, generated sqlc files and all files changed above.

**Interfaces:** main owns only the shared pool until real use-case consumers need repositories; no blank assignments, unused constructors, startup sample writes or universal repository facade.

- [ ] Remove old NewRepository setup and unused sqlc import/queries variable from main; delete the old repository file after confirming its behavior is in WorkspaceRepository. Preserve existing server/relay behavior and unrelated edits.
- [ ] Run `make generate` twice and confirm no generated differences on the second run. Never hand-edit generated files.
- [ ] Run `goose -dir db/migrations validate`, `go test ./...`, and `go vet ./...`.
- [ ] Run `TEST_DATABASE_URL=<isolated-local-test-URL> go test -race ./internal/adapters/storage -count=1 -v` and confirm integration tests actually execute, not skip. Do not log the URL or read project .env.
- [ ] Review the focused diff for tenant filters, error causes, JSON/nullable mappings, transaction release and source identity immutability. Use whitespace checks without modifying unrelated existing whitespace.
- [ ] Report changed files, verification results and limitations. Leave changes uncommitted unless requested.
