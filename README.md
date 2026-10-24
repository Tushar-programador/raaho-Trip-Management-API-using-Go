# Trip Management API

A production-oriented REST service in Go for managing the lifecycle of transport trips: create a trip, assign it, move it through transit to completion (or cancel it), and query trips with pagination and filtering.

The interesting part is not the CRUD. It is that **business rules hold under concurrency and across multiple API instances**: status changes are validated against the *persisted* state while the row is locked in PostgreSQL, trip numbers are unique by database constraint, and `POST /trips` supports durable, race-safe `Idempotency-Key` retries. Every trip endpoint requires a revocable per-client API key, and the API process connects to PostgreSQL as a role that cannot alter or drop the schema.

---

## Contents

- [Features](#features)
- [Technology stack](#technology-stack)
- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Project structure](#project-structure)
- [Database design](#database-design)
- [Trip lifecycle](#trip-lifecycle)
- [API reference](#api-reference)
- [Concurrency control](#concurrency-control)
- [Duplicate requests and idempotency](#duplicate-requests-and-idempotency)
- [Errors](#errors)
- [Configuration](#configuration)
- [Database migrations](#database-migrations)
- [Running with Docker Compose](#running-with-docker-compose)
- [Running on the host](#running-on-the-host)
- [Testing](#testing)
- [OpenAPI documentation](#openapi-documentation)
- [Security and production readiness](#security-and-production-readiness)
- [Design decisions and trade-offs](#design-decisions-and-trade-offs)
- [Audit history (planned)](#audit-history-planned)
- [Known limitations and future work](#known-limitations-and-future-work)

---

## Features

| Capability | How it is delivered |
|---|---|
| Five REST endpoints | `POST /trips`, `GET /trips`, `GET /trips/{id}`, `PATCH /trips/{id}/status`, `DELETE /trips/{id}` |
| Strict state machine | Transition table in the domain layer, enforced inside a locking transaction |
| Safe concurrent updates | `SELECT ... FOR UPDATE` in PostgreSQL, so it also holds across replicas |
| Unique trip numbers | `UNIQUE` constraint; concurrent duplicates resolved by the database |
| Idempotent creation | Optional `Idempotency-Key` header, stored durably in PostgreSQL, scoped per client |
| Authentication | Per-client API keys (`Authorization: Bearer`), stored only as SHA-256 hashes, revocable instantly |
| Least-privilege database access | The API's role reads and writes rows but cannot alter, drop or truncate anything |
| Pagination and filtering | `page`, `page_size` (max 100) and `status`, deterministic ordering, index-backed |
| Strict input handling | Unknown fields, malformed JSON, wrong content type and oversized bodies are rejected |
| Consistent errors | One error envelope with stable codes and a request ID; no internals leaked |
| Operability | Structured JSON logs, request IDs, panic recovery, timeouts, liveness and readiness probes, graceful shutdown |
| Delivery | Multi-stage Docker build (distroless, non-root), Compose stack with health-gated startup |

## Technology stack

| Concern | Choice |
|---|---|
| Language | Go 1.26 |
| HTTP router | [chi v5](https://github.com/go-chi/chi) |
| Database | PostgreSQL 16 |
| Driver / pool | [pgx v5](https://github.com/jackc/pgx) with `pgxpool` |
| Migrations | [golang-migrate v4](https://github.com/golang-migrate/migrate), with migrations embedded in the binary |
| IDs | [google/uuid](https://github.com/google/uuid) (UUIDv7) |
| Logging | `log/slog` (JSON in containers, text locally) |
| Tests | `testing`, `httptest`, real PostgreSQL |
| API docs | OpenAPI 3.0.3 |
| Packaging | Docker (distroless static, non-root) and Docker Compose |

There are four direct dependencies in total. There is no web framework, ORM, or DI container.

## Quick start

Prerequisites: **Docker** with Compose v2. For running outside Docker you also need **Go 1.26+**. **make** is optional, and every target has a plain-command equivalent below.

```bash
git clone https://github.com/Tushar-programador/trip-management-api.git
cd trip-management-api
cp .env.example .env            # local defaults (see the note below if 5432/8080 are taken)

make docker-up                  # or: docker compose up -d --build --wait
curl http://localhost:8080/readyz
# {"status":"ready"}

# Create an API key (printed once) and call the API with it
export API_KEY=$(docker compose run --rm -T --no-deps --entrypoint /app/apikey migrate create local-dev)
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/trips
# {"data":[],"pagination":{"page":1,"page_size":20,"total_items":0,"total_pages":0}}
```

`make api-key NAME=local-dev` runs the same key-creation command. On Git Bash for Windows, prefix `docker compose run` commands with `MSYS_NO_PATHCONV=1`, so that `/app/apikey` is not rewritten into a Windows path.

`docker-up` starts PostgreSQL, waits for it to be healthy, runs the migrations once in a separate container, and only then starts the API. The command returns when the API's health check passes.

If port 5432 or 8080 is already in use, set `POSTGRES_PORT` and `API_PORT` in `.env`. If you change `POSTGRES_PORT`, also update the port in the four `*DATABASE_URL` variables; the host-side commands (`make run`, `make migrate-up`, `make test-integration`) use those URLs.

Stop with `make docker-down` (or `docker compose down`). Add `-v` to `docker compose down` to also delete the database volume.

---

## Architecture

A pragmatic layered design. Each layer depends only on the one below it, and dependencies are wired explicitly in `main` (no globals).

```mermaid
flowchart LR
    C[Client] -->|HTTP| MW

    subgraph API process
      MW[Middleware<br/>request ID · logging · recovery<br/>body limit · timeout · API-key auth] --> H[Handler<br/>parse · validate input · map errors]
      H --> S[Service<br/>use cases · normalization · idempotency]
      S --> D[Domain<br/>Trip · Status · transitions · errors]
      S --> R[Repository<br/>SQL · transactions · locking]
    end

    R -->|pgxpool| PG[(PostgreSQL<br/>constraints · row locks)]
```

| Layer | Package | Responsibility |
|---|---|---|
| Entry point | `cmd/api` | Load config, connect, verify schema version, start server, graceful shutdown |
| Middleware | `internal/middleware` | Request ID, panic recovery, access logs, request timeout, body size limit |
| Auth | `internal/auth` | API-key generation and hashing; bearer middleware that resolves a key to a client |
| Handler | `internal/handler` | Routing, strict JSON decoding, query parsing, response and error serialization |
| Service | `internal/service` | Normalize and validate input, generate IDs, build idempotency fingerprints |
| Domain | `internal/domain` | Trip model, statuses, transition table, validation rules, domain errors |
| Repository | `internal/repository` | All SQL, transaction boundaries, row locks, driver-to-domain error mapping |
| Platform | `internal/platform/*` | Connection pool, schema check, migrator, logger |
| Admin CLIs | `cmd/migrate`, `cmd/apikey` | Schema migrations and API-key management, run as the schema owner |

There are two small interfaces, `handler.TripService` and `auth.ClientStore`. Both are declared at the consumer so handler and middleware tests can run without a database. Everything else uses concrete types.

### Request flow: `PATCH /trips/{id}/status`

1. Middleware assigns a request ID, caps the body, and bounds the request context with `REQUEST_TIMEOUT`. On `/trips` routes it then resolves the bearer API key to a client, or returns `401`.
2. The handler validates the UUID and strictly decodes `{"status": ...}`.
3. The service checks that `status` is one of the five known values (`400 INVALID_STATUS` otherwise).
4. The repository opens a transaction, locks the row, asks the domain whether *current → requested* is allowed, updates, and commits.
5. Domain errors are mapped to HTTP status codes and error codes in exactly one place (`handler/errors.go`).

## Project structure

```text
.
├── api/
│   ├── openapi.yaml            # OpenAPI 3.0.3 contract
│   └── embed.go                # embeds the spec; served at GET /openapi.yaml
├── cmd/
│   ├── api/main.go             # HTTP server (+ `api healthcheck` probe)
│   ├── migrate/main.go         # migration CLI: up | down | version | force N
│   └── apikey/main.go          # API-key admin CLI: create | list | revoke
├── deploy/postgres/init/       # first start: trips_test database, trip_api login role
├── internal/
│   ├── auth/                   # API keys: generation, hashing, bearer middleware
│   ├── config/                 # env-based config with validation
│   ├── domain/                 # Trip, Status, transitions, validation, errors
│   ├── handler/                # routes, JSON I/O, pagination, error mapping
│   ├── middleware/             # request ID, recovery, logging, timeout, body limit
│   ├── platform/
│   │   ├── database/           # pgxpool, schema-version check, migrator
│   │   └── logger/             # slog setup
│   ├── repository/             # PostgreSQL queries and transactions
│   └── service/                # use cases
├── migrations/                 # versioned SQL (embedded into the binaries)
├── tests/                      # HTTP integration and concurrency tests (real PostgreSQL)
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── .env.example
```

---

## Database design

### `trips`

| Column | Type | Constraints |
|---|---|---|
| `id` | `UUID` | `PRIMARY KEY` (UUIDv7, generated in Go) |
| `trip_number` | `VARCHAR(64)` | `NOT NULL`, `UNIQUE`, not blank |
| `source` | `VARCHAR(255)` | `NOT NULL`, not blank |
| `destination` | `VARCHAR(255)` | `NOT NULL`, not blank |
| `driver_name` | `VARCHAR(100)` | `NOT NULL`, not blank |
| `status` | `VARCHAR(20)` | `NOT NULL`, `CHECK` in the five statuses, default `CREATED` |
| `created_at` | `TIMESTAMPTZ` | `NOT NULL`, set by PostgreSQL `now()` |
| `updated_at` | `TIMESTAMPTZ` | `NOT NULL`, `CHECK (updated_at >= created_at)` |

The rule is that anything expressible as a constraint is enforced by the database, not only by Go code. A buggy code path or a manual `psql` session still cannot store an unknown status, a blank field, or a duplicate trip number. `TestDatabaseConstraintsBackTheRules` checks this directly.

### Indexes

| Index | Serves |
|---|---|
| `trips_pkey (id)` | Lookups, updates and deletes by ID |
| `trips_trip_number_key (trip_number)` | Created by the `UNIQUE` constraint; no separate index is added |
| `trips_status_created_at_id_idx (status, created_at DESC, id DESC)` | `GET /trips?status=X`, ordered, with no sort step |
| `trips_created_at_id_idx (created_at DESC, id DESC)` | Unfiltered `GET /trips`, ordered, with no sort step |

Both list queries were checked with `EXPLAIN` on 50,000 seeded rows. Each uses an `Index Scan` on its index under the `LIMIT` with no `Sort` node.

### `idempotency_keys`

| Column | Purpose |
|---|---|
| `client_id`, `scope`, `idempotency_key` | Composite `PRIMARY KEY`: per client (`REFERENCES api_clients`) and per operation (`POST /trips`) |
| `request_hash` | SHA-256 fingerprint of the normalized request |
| `trip_id`, `trip_snapshot` | The trip exactly as first returned (`JSONB`), used for replays |
| `created_at`, `expires_at` | Retention; expired keys can be reclaimed |

A `CHECK` constraint ensures a key row either has a complete result or none. An index on `expires_at` supports purging expired keys.

Timestamps are `TIMESTAMPTZ`, written by PostgreSQL and always returned in UTC (`2026-10-24T06:39:37.86131Z`).

### `api_clients`

| Column | Purpose |
|---|---|
| `id`, `name` | Client identity, recorded as `client_id` in access logs |
| `key_hash` | SHA-256 of the API key, `UNIQUE`; this is also the index for the per-request lookup |
| `key_prefix` | First 12 characters of the key, to recognise it in listings |
| `created_at`, `revoked_at` | A non-null `revoked_at` disables the key on the next request |

The plaintext key is never stored. It is shown once by `apikey create`.

### Database roles

The API never connects as the schema owner.

| Role | Used by | Can do |
|---|---|---|
| Owner (`POSTGRES_USER`, via `ADMIN_DATABASE_URL`) | `migrate`, `apikey` | Everything: DDL, migrations, issuing and revoking API keys |
| `trip_api` (via `DATABASE_URL`) | The API | `SELECT/INSERT/UPDATE/DELETE` on `trips` and `idempotency_keys`; `SELECT` only on `api_clients` and `schema_migrations`. No `ALTER`, `DROP`, `TRUNCATE` or `CREATE`. |

A bug or SQL injection in the API therefore cannot alter or destroy the schema, rewrite the migration history, or mint or un-revoke API keys.

- **Grants are in migrations** (`000003_grant_api_role`). Each later table grants `trip_api` exactly what it needs, so privileges are reviewed together with the schema change.
- **The credential is not.** In Compose, `deploy/postgres/init/02-create-api-role.sh` creates `trip_api` with `LOGIN` and `API_DB_PASSWORD` when the volume is first initialised. Elsewhere, the migration creates the role as `NOLOGIN` if it is missing, and an operator enables it with `ALTER ROLE trip_api LOGIN PASSWORD '...'`.
- **The API checks its own privileges at startup.** If it is connected as a superuser or the table owner, it logs a warning.
- **Tests enforce this.** `TestAPIRoleCannotChangeSchema` asserts that every DDL statement is refused with `42501`. The rest of the integration suite runs through the API on this role.

---

## Trip lifecycle

```mermaid
stateDiagram-v2
    [*] --> CREATED
    CREATED --> ASSIGNED
    CREATED --> CANCELLED
    ASSIGNED --> IN_TRANSIT
    ASSIGNED --> CANCELLED
    IN_TRANSIT --> COMPLETED
    COMPLETED --> [*]
    CANCELLED --> [*]
```

| From | Allowed to |
|---|---|
| `CREATED` | `ASSIGNED`, `CANCELLED` |
| `ASSIGNED` | `IN_TRANSIT`, `CANCELLED` |
| `IN_TRANSIT` | `COMPLETED` |
| `COMPLETED` | none (terminal) |
| `CANCELLED` | none (terminal) |

- Every other transition, including "to the same status", returns **`409 INVALID_STATUS_TRANSITION`**.
- An unknown or wrong-case status value (`"DONE"`, `"assigned"`) is a malformed request and returns **`400 INVALID_STATUS`**.
- Status can only change through `PATCH /trips/{id}/status`. `POST /trips` rejects a `status` field as an unknown field.

The transition table lives in `internal/domain/trip.go`. Its unit test checks all 25 `(from, to)` pairs, so adding a transition by accident fails the build.

---

## API reference

| Method | Path | Success | Errors |
|---|---|---|---|
| `POST` | `/trips` | `201` trip (+`Location`) | `400`, `401`, `409`, `413`, `415`, `500`, `503` |
| `GET` | `/trips` | `200` page | `400`, `401`, `500`, `503` |
| `GET` | `/trips/{id}` | `200` trip | `400`, `401`, `404`, `500`, `503` |
| `PATCH` | `/trips/{id}/status` | `200` trip | `400`, `401`, `404`, `409`, `413`, `415`, `500`, `503` |
| `DELETE` | `/trips/{id}` | `204` | `400`, `401`, `404`, `500`, `503` |
| `GET` | `/healthz` | `200` liveness | |
| `GET` | `/readyz` | `200` readiness | `503` |
| `GET` | `/openapi.yaml` | `200` OpenAPI document | |

Request bodies must be `Content-Type: application/json`. Every response carries an `X-Request-ID` header.

### Authentication

Every `/trips` request must carry an API key:

```
Authorization: Bearer tma_<43 base64url characters>
```

- **Errors.** A missing, malformed, unknown or revoked key returns `401 UNAUTHENTICATED` with `WWW-Authenticate: Bearer`. The cases are deliberately indistinguishable.
- **Database outage.** If the key cannot be checked because the database is down, the response is `503`, not a misleading `401`.
- **Public endpoints.** `/healthz`, `/readyz` and `/openapi.yaml` need no key.

Keys are managed with `apikey`, which connects as the schema owner through `ADMIN_DATABASE_URL`:

| Command | Effect |
|---|---|
| `apikey create NAME` | Registers a client and prints its key. The key is shown once and only its SHA-256 hash is stored. It is the only output on stdout, so `KEY=$(apikey create NAME)` captures it. |
| `apikey list` | Lists clients: ID, name, key prefix, created and revoked times |
| `apikey revoke ID` | Revokes the key. The next request using it gets `401`. |

```bash
# Docker Compose (the image contains the binary)
docker compose run --rm -T --no-deps --entrypoint /app/apikey migrate create billing-service
docker compose run --rm -T --no-deps --entrypoint /app/apikey migrate list
# On the host, with ADMIN_DATABASE_URL set (e.g. from .env)
go run ./cmd/apikey revoke 0192f3a4-5b6c-7d8e-9f01-23456789abcd
```

The examples below assume `API_KEY` holds a key, as in [Quick start](#quick-start).

### Field rules for `POST /trips`

| Field | Required | Max length (characters) | Notes |
|---|---|---|---|
| `trip_number` | yes | 64 | Unique, case-sensitive |
| `source` | yes | 255 | |
| `destination` | yes | 255 | |
| `driver_name` | yes | 100 | |

- Leading and trailing whitespace is trimmed before validation and storage, so `"  "` counts as missing and `" T-1 "` is the same trip number as `"T-1"`.
- Control characters (newline, NUL, and so on) are rejected.
- All field errors are reported at once.

### Create a trip

```bash
curl -i -X POST http://localhost:8080/trips \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"trip_number":"TRIP-1001","source":"Mumbai","destination":"Pune","driver_name":"Ravi Kumar"}'
```

```http
HTTP/1.1 201 Created
Content-Type: application/json; charset=utf-8
Location: /trips/01a16ca2-e784-7927-8432-76a21ee80157
X-Request-Id: 7afefb5c-4bf2-4e27-942d-26944806e63d
```

```json
{
  "id": "01a16ca2-e784-7927-8432-76a21ee80157",
  "trip_number": "TRIP-1001",
  "source": "Mumbai",
  "destination": "Pune",
  "driver_name": "Ravi Kumar",
  "status": "CREATED",
  "created_at": "2026-10-24T06:39:37.86131Z",
  "updated_at": "2026-10-24T06:39:37.86131Z"
}
```

A duplicate trip number returns `409`:

```json
{"error":{"code":"TRIP_NUMBER_EXISTS","message":"A trip with this trip_number already exists","request_id":"15abad01-29c6-498a-a706-85cdcacdca53"}}
```

Missing or blank fields return `400`, with every problem listed:

```bash
curl -X POST http://localhost:8080/trips -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"trip_number":"  ","source":"Mumbai"}'
```

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Request validation failed",
    "request_id": "a9164b0d-51aa-444d-afc4-523e0d24c9cc",
    "details": [
      {"field": "trip_number", "message": "is required"},
      {"field": "destination", "message": "is required"},
      {"field": "driver_name", "message": "is required"}
    ]
  }
}
```

### Get a trip

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/trips/01a16ca2-e784-7927-8432-76a21ee80157
```

An invalid UUID returns `400 VALIDATION_ERROR` with `{"field":"id","message":"must be a valid UUID"}`. Only the canonical 36-character form is accepted. A well-formed but unknown ID returns `404`:

```json
{"error":{"code":"TRIP_NOT_FOUND","message":"Trip not found","request_id":"eea2d24a-f1cb-432b-a6e0-ea3f96736c7b"}}
```

### List trips (pagination and filtering)

| Parameter | Default | Allowed | On invalid input |
|---|---|---|---|
| `page` | `1` | integer `1`–`1000000` | `400 VALIDATION_ERROR` |
| `page_size` | `20` | integer `1`–`100` | `400 VALIDATION_ERROR` |
| `status` | (none) | one of the five statuses (exact case) | `400 INVALID_STATUS` |

Results are ordered by `created_at DESC, id DESC`, which is newest first and stable even for equal timestamps. Out-of-range values are rejected rather than silently clamped, so client bugs surface. A page past the end returns `"data": []` with correct totals.

```bash
curl -H "Authorization: Bearer $API_KEY" 'http://localhost:8080/trips?page=1&page_size=20&status=ASSIGNED'
```

```json
{
  "data": [
    {
      "id": "01a16ca2-e784-7927-8432-76a21ee80157",
      "trip_number": "TRIP-1001",
      "source": "Mumbai",
      "destination": "Pune",
      "driver_name": "Ravi Kumar",
      "status": "ASSIGNED",
      "created_at": "2026-10-24T06:39:37.86131Z",
      "updated_at": "2026-10-24T06:39:38.660424Z"
    }
  ],
  "pagination": {"page": 1, "page_size": 20, "total_items": 1, "total_pages": 1}
}
```

```bash
curl -H "Authorization: Bearer $API_KEY" 'http://localhost:8080/trips?page=0&page_size=500'
```

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Request validation failed",
    "request_id": "60434b67-d8c9-4cb1-948b-ac11472db34d",
    "details": [
      {"field": "page", "message": "must be an integer between 1 and 1000000"},
      {"field": "page_size", "message": "must be an integer between 1 and 100"}
    ]
  }
}
```

The count and the page are read in a single `REPEATABLE READ` read-only transaction, so `total_items` always agrees with `data`, even while trips are being written.

### Change status

```bash
curl -X PATCH http://localhost:8080/trips/01a16ca2-e784-7927-8432-76a21ee80157/status \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' -d '{"status":"ASSIGNED"}'
```

This returns `200` with the updated trip (`"status":"ASSIGNED"` and a new `updated_at`). Skipping a step returns `409`:

```bash
curl -X PATCH http://localhost:8080/trips/01a16ca2-e784-7927-8432-76a21ee80157/status \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' -d '{"status":"COMPLETED"}'
```

```json
{"error":{"code":"INVALID_STATUS_TRANSITION","message":"cannot transition trip from ASSIGNED to COMPLETED","request_id":"0d3c0a67-b064-4929-8164-d40114c1bbd7"}}
```

```json
{"error":{"code":"INVALID_STATUS","message":"invalid status \"DONE\": must be one of CREATED, ASSIGNED, IN_TRANSIT, COMPLETED, CANCELLED","request_id":"6e7e58a0-cc07-4d53-ae8a-ac05c8f0f2a7"}}
```

### Delete a trip

```bash
curl -i -X DELETE -H "Authorization: Bearer $API_KEY" http://localhost:8080/trips/01a16ca2-e784-7927-8432-76a21ee80157
# HTTP/1.1 204 No Content
```

Deleting again returns `404 TRIP_NOT_FOUND`. Success is never reported unless a row was actually removed (`RowsAffected() == 1`).

Deletion policy:

- This service uses **physical deletion**.
- **Trips in any status can be deleted, including terminal ones.** Deletion is treated as an administrative correction, not a lifecycle step, so it is not part of the state machine.
- A deleted trip's `trip_number` becomes available again.
- If a delete and a status update race on the same trip, they serialize on the row lock. The loser sees either the updated trip (delete then succeeds) or no row (update returns `404`).
- See [future work](#known-limitations-and-future-work) for the soft-delete alternative.

---

## Concurrency control

**Requirement:** two simultaneous status updates must never let one of them bypass the transition rules because it read stale state. This must hold with any number of API replicas.

**Mechanism:** pessimistic row locking inside a PostgreSQL transaction (`internal/repository/trip_repository.go`, `UpdateStatus`).

```sql
BEGIN;
SELECT status FROM trips WHERE id = $1 FOR UPDATE;   -- 1. lock the row (no row -> 404)
-- 2. domain.ValidateTransition(current, requested) runs here, in Go, while the lock is held
UPDATE trips SET status = $2, updated_at = greatest(now(), updated_at)
 WHERE id = $1 RETURNING ...;                         -- 3. write
COMMIT;                                               -- 4. release
```

```mermaid
sequenceDiagram
    participant A as Request A (→ ASSIGNED)
    participant DB as PostgreSQL
    participant B as Request B (→ IN_TRANSIT)
    A->>DB: BEGIN; SELECT ... FOR UPDATE
    DB-->>A: status = CREATED (row locked)
    B->>DB: BEGIN; SELECT ... FOR UPDATE
    Note over B,DB: blocks on A's row lock
    A->>A: CREATED → ASSIGNED allowed
    A->>DB: UPDATE; COMMIT
    DB-->>B: lock granted, status = ASSIGNED (fresh)
    B->>B: ASSIGNED → IN_TRANSIT allowed
    B->>DB: UPDATE; COMMIT
```

**Why this is safe:**

- The status that is validated is the status that is overwritten. No other transaction can change the row between the `SELECT ... FOR UPDATE` and the `UPDATE`, because both happen under one row lock.
- Under `READ COMMITTED`, a transaction blocked on `FOR UPDATE` re-reads the **latest committed** row version once the lock is granted, so validation never runs on a stale value.
- The lock lives in PostgreSQL, not in process memory, so it is equally correct with one replica or fifty. There are no application mutexes.
- Locks are per row, so updates to different trips never contend. Each transaction touches exactly one row, so there are no lock-ordering deadlocks.
- `pgx.BeginFunc` rolls back on any error or panic.
- Waiting is bounded on the server, not just in Go. Every connection sets PostgreSQL's `lock_timeout` (`DB_LOCK_TIMEOUT`, 2s) and `statement_timeout` (`DB_STATEMENT_TIMEOUT`, 5s), and both are below `REQUEST_TIMEOUT`.
  - A request stuck behind a long-held lock gets a retryable `503`.
  - PostgreSQL removes the waiter from the lock queue.
  - A cancelled Go context alone only closes the socket, and that would leave the server-side backend waiting. `TestStatusUpdateLockTimeout` checks this.
- Transition logic stays in the domain package. The repository only supplies the transactional frame.

**How it is tested** (`tests/concurrency_test.go`, real PostgreSQL, independent HTTP requests, no sleeps used as synchronization):

| Test | What it proves |
|---|---|
| `TestStatusUpdateWaitsForRowLock` | **Deterministic.** A competing transaction locks the row and changes the status without committing. The test confirms through `pg_stat_activity` that the HTTP request is blocked on the lock, commits, and asserts that the request validated against the new status. One case would be wrongly rejected from stale `CREATED`; the other would wrongly resurrect a cancelled trip. |
| `TestConcurrentIdenticalTransitions` | 25 simultaneous `CREATED → ASSIGNED` requests, over 15 trips: exactly one `200` per trip, the rest `409`. |
| `TestConcurrentCompetingTransitions` | 16 mixed transitions fired at once, over 20 trips. Since the state machine is acyclic, the successful transitions must form one valid path from `CREATED`, the stored status must be its end, and every rejection must be a clean `409`. |
| `TestConcurrentAssignVersusInTransit` | The exact scenario from the requirements, repeated 30 times: `IN_TRANSIT` is accepted only if `ASSIGNED` has already committed. |

Goroutines are released together through a closed channel, so they start as close to simultaneously as possible. As a check that these tests have teeth, removing `FOR UPDATE` from the repository makes the first three fail, for example with 23 of 25 identical requests "succeeding", and with cancelled trips ending up `ASSIGNED`.

---

## Duplicate requests and idempotency

There are two independent layers of protection.

### 1. Unique trip numbers (always on)

`trip_number` has a `UNIQUE` constraint. There is no "check, then insert" in application code. The insert itself is the check, so simultaneous duplicates are resolved by PostgreSQL. The unique violation (`23505` on `trips_trip_number_key`) is mapped to `409 TRIP_NUMBER_EXISTS`. `TestConcurrentDuplicateTripNumbers` fires 25 identical creates at once and asserts exactly one `201`, 24 `409`s, and one row.

### 2. `Idempotency-Key` header (optional, fully implemented)

This makes retries of `POST /trips` safe, for example after a timeout where the client cannot tell whether the trip was created.

```bash
curl -i -X POST http://localhost:8080/trips \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 8d5f6a8e-2b1c-4c3e-9f7a-1b2c3d4e5f60' \
  -d '{"trip_number":"TRIP-1001","source":"Mumbai","destination":"Pune","driver_name":"Ravi Kumar"}'
```

| Situation | Result |
|---|---|
| New key | Trip created, `201`. Key and result stored in the same transaction. |
| Same key, same request | Original `201` response replayed with header `Idempotent-Replayed: true`. No new trip is created. |
| Same key, different request | `409 IDEMPOTENCY_KEY_REUSED`, nothing created |
| Same key while the first request is still in flight | Waits for the first one, then replays its result (or proceeds if it failed) |
| Original request failed (for example a duplicate trip number) | Nothing is remembered; the key can be reused with a corrected body |
| Key older than `IDEMPOTENCY_TTL` (default `24h`) | Treated as new |
| Header present but empty, longer than 255, or not printable ASCII | `400 VALIDATION_ERROR` |
| Same key used by a different client | Independent: each client has its own key space |

**How it works.** The key row is inserted at the start of the same transaction that inserts the trip:

```sql
INSERT INTO idempotency_keys (client_id, scope, idempotency_key, request_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, now(), now() + make_interval(secs => $5))
ON CONFLICT (client_id, scope, idempotency_key) DO UPDATE SET ...   -- reclaim only if expired
 WHERE idempotency_keys.expires_at <= now();
```

- If 1 row is affected, this request owns the key. It inserts the trip, stores the trip snapshot on the key row, and commits atomically.
- If 0 rows are affected, the key is live and committed. The request compares fingerprints, then replays the snapshot or returns `409`.
- If another transaction holds the same key uncommitted, PostgreSQL **blocks this insert on the primary-key index** until that transaction ends. There is no window in which two requests can both believe they own the key, and no "pending" state to clean up after crashes.

Details:

- **Fingerprint.** The fingerprint is SHA-256 over the normalized input, so JSON key order and surrounding whitespace do not make two semantically equal requests "different".
- **Snapshot replay.** The replay returns the trip as it was when created, even if it has since changed status or been deleted. This matches the semantics of a retried request.
- **Scope.** Keys are unique per authenticated client and operation, through the primary key `(client_id, scope, idempotency_key)`. Two clients can use the same key without colliding, and neither can obtain the other's result through it (`TestIdempotencyKeysAreScopedPerClient`).

Tests: `tests/idempotency_test.go`. They cover replay, key reuse with a different body, keys not consumed by failed requests, expiry, and two concurrency tests: 25 simultaneous identical requests produce one trip and 24 replays, and racing two different bodies under one key produces exactly one winner.

---

## Errors

Every error uses one envelope:

```json
{
  "error": {
    "code": "TRIP_NOT_FOUND",
    "message": "Trip not found",
    "request_id": "eea2d24a-f1cb-432b-a6e0-ea3f96736c7b"
  }
}
```

`code` is stable and machine-readable; `message` is for humans. `details` appears only for `VALIDATION_ERROR`. Quote `request_id` (also in the `X-Request-ID` header) when reporting problems; it matches the server log line.

| HTTP | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | Missing, blank, too long or invalid fields; invalid UUID; bad `page`/`page_size`; bad `Idempotency-Key` |
| 400 | `INVALID_JSON` | Malformed JSON; empty or non-object body (including `null`); unknown, case-variant or duplicate field; wrong type; multiple JSON values |
| 400 | `INVALID_STATUS` | Status value that is not one of the five statuses |
| 401 | `UNAUTHENTICATED` | Missing, malformed, unknown or revoked API key on a `/trips` route (with `WWW-Authenticate: Bearer`) |
| 404 | `TRIP_NOT_FOUND` | No trip with this ID |
| 404 | `ROUTE_NOT_FOUND` | Unknown path |
| 405 | `METHOD_NOT_ALLOWED` | Known path, wrong method |
| 409 | `TRIP_NUMBER_EXISTS` | `trip_number` already used |
| 409 | `INVALID_STATUS_TRANSITION` | Transition not allowed from the current status |
| 409 | `IDEMPOTENCY_KEY_REUSED` | Key reused with a different request body |
| 413 | `PAYLOAD_TOO_LARGE` | Body larger than `MAX_BODY_BYTES` |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | `Content-Type` is not `application/json` |
| 500 | `INTERNAL_ERROR` | Unexpected failure; details are logged, never returned |
| 503 | `SERVICE_UNAVAILABLE` | Database unreachable or timed out, trip row locked longer than `DB_LOCK_TIMEOUT`, or the server is draining for shutdown |

`422` is deliberately not used. All client input problems are `400`, which keeps the contract simple and consistent.

SQL text, driver messages, constraint names and stack traces never reach clients. Unknown errors become a generic `500` and are logged with the request ID.

If a client disconnects before its response is ready, no response is sent. The request is logged at `info` with status `499` (the nginx convention), so client hang-ups do not count as server errors.

---

## Configuration

All configuration comes from environment variables. The values are validated at startup, and every problem is reported at once.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | **required** | PostgreSQL URL for the API, as the least-privilege `trip_api` role, e.g. `postgres://trip_api:pass@host:5432/db?sslmode=disable` |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Max time to read request headers (slowloris protection) |
| `HTTP_READ_TIMEOUT` | `10s` | Max time to read the whole request |
| `HTTP_WRITE_TIMEOUT` | `15s` | Max time to write the response |
| `HTTP_IDLE_TIMEOUT` | `60s` | Keep-alive idle timeout |
| `REQUEST_TIMEOUT` | `10s` | Context deadline for handlers and their DB queries; must be less than `HTTP_WRITE_TIMEOUT` |
| `SHUTDOWN_TIMEOUT` | `20s` | Grace period for in-flight requests on SIGINT/SIGTERM |
| `SHUTDOWN_DRAIN_DELAY` | `0s` | Time to keep serving with `/readyz` failing before closing the listener, so load balancers can deregister the instance (e.g. `5s`); counts toward and must be less than `SHUTDOWN_TIMEOUT` |
| `MAX_BODY_BYTES` | `65536` | Request body limit (`413` above it) |
| `DB_MAX_CONNS` | `10` | Connection pool size |
| `DB_CONNECT_TIMEOUT` | `30s` | How long startup retries reaching the database before failing |
| `DB_STATEMENT_TIMEOUT` | `5s` | PostgreSQL `statement_timeout` per connection; must be less than `REQUEST_TIMEOUT` |
| `DB_LOCK_TIMEOUT` | `2s` | PostgreSQL `lock_timeout` per connection (`503` when exceeded); must not exceed `DB_STATEMENT_TIMEOUT` |
| `IDEMPOTENCY_TTL` | `24h` | How long an `Idempotency-Key` is remembered |

Other variables:

| Variable | Used by | Description |
|---|---|---|
| `ADMIN_DATABASE_URL` | `migrate`, `apikey` | Owner-role URL; required |
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | Compose | Owner credentials and database name |
| `API_DB_PASSWORD` | Compose | Password given to `trip_api` when the volume is first created (default `trip_api`) |
| `POSTGRES_PORT`, `API_PORT` | Compose | Host ports (defaults `5432`, `8080`) |
| `TEST_DATABASE_URL` | Integration tests | Owner URL for a dedicated test database |
| `TEST_APP_DATABASE_URL` | Integration tests | Optional `trip_api` URL; when set, the API under test runs as that role |

## Database migrations

Migrations are plain, versioned SQL files in `migrations/`. They are embedded into the binaries, so the files that are tested are the files that are deployed.

| Command | Make target | Effect |
|---|---|---|
| `go run ./cmd/migrate up` | `make migrate-up` | Apply all pending migrations |
| `go run ./cmd/migrate down` | `make migrate-down` | Roll back the latest migration |
| `go run ./cmd/migrate version` | `make migrate-version` | Show current version and dirty flag |
| `go run ./cmd/migrate force N` | | Mark version `N` clean after fixing a failed migration |

All of them connect with `ADMIN_DATABASE_URL`, the schema owner. The API's own role cannot run DDL.

> **Upgrading an existing local volume:** `trip_api` is created by the init script, which only runs on a fresh volume. If your volume predates the role, either recreate it with `docker compose down -v`, which deletes the data, or run `ALTER ROLE trip_api LOGIN PASSWORD 'trip_api'` as the owner after migrating.

**Order of operations:** migrate first, then start the API.

- **Migrations never run inside API replicas.** This avoids N replicas racing to alter the schema on startup. golang-migrate also holds a PostgreSQL advisory lock while migrating, so an accidental concurrent run is serialized.
- **The API verifies the schema at startup.** It reads `schema_migrations` and refuses to start if the schema is behind the version compiled into the binary, or is dirty:

  ```
  fatal: database: read schema version (have migrations been run?): ERROR: relation "schema_migrations" does not exist (SQLSTATE 42P01)
  ```

In Compose this ordering is automatic: the `migrate` service must exit successfully before `api` starts.

## Running with Docker Compose

```bash
cp .env.example .env     # optional; defaults work without it
make docker-up           # docker compose up -d --build --wait
make docker-logs         # docker compose logs -f api
make docker-down         # docker compose down
make api-key NAME=my-app # create an API key (printed once)
make api-keys            # list API clients
```

| Service | Role | Health |
|---|---|---|
| `postgres` | PostgreSQL 16 with a named volume (`pgdata`) | `pg_isready` |
| `migrate` | Runs `/app/migrate up` once, then exits | Must exit `0` |
| `api` | The HTTP API | `/app/api healthcheck` probes `/readyz` |

If ports 5432 or 8080 are already taken, change the host ports without touching any file:

```bash
POSTGRES_PORT=55432 API_PORT=18080 docker compose up -d --build --wait
```

The image is a multi-stage build: `golang:1.26-alpine` compiles static binaries, and the runtime is `gcr.io/distroless/static-debian12:nonroot`. That image has no shell or package manager and runs as non-root UID 65532.

## Running on the host

```bash
cp .env.example .env     # DATABASE_URL (trip_api) and ADMIN_DATABASE_URL (owner) point at localhost:5432
make db-up               # docker compose up -d --wait postgres
make migrate-up          # go run ./cmd/migrate up  (as the owner)
make run                 # go run ./cmd/api
```

The Makefile loads `.env` automatically. Without `make`, export the variables from `.env` yourself and run the `go` commands shown.

---

## Testing

| Suite | Location | Needs DB | Covers |
|---|---|---|---|
| Domain unit tests | `internal/domain` | no | All 25 transition pairs, terminal states, status parsing, field validation and length limits (in characters), trimming, fingerprints, idempotency-key rules |
| Handler unit tests | `internal/handler` | no | Malformed, empty, unknown-field, wrong-type and oversized bodies; wrong content type; invalid UUID forms; pagination defaults, bounds and invalid values; error-to-HTTP mapping; panic recovery without leaks; request-ID propagation; readiness while draining |
| Config unit tests | `internal/config` | no | Defaults, aggregated errors, timeout invariants |
| Auth unit tests | `internal/auth`, `internal/middleware` | no | Key format and hashing, bearer parsing, every accept/reject path of the middleware, `client_id` in access logs |
| Repository unit tests | `internal/repository` | no | Driver-to-domain error mapping: constraint names, connection-loss and timeout classes, client cancellation |
| Integration tests | `tests/` | **yes** | Every endpoint end to end through the real router, service, repository and PostgreSQL, plus DB constraints, concurrency, idempotency, authentication and revocation, per-client idempotency, and the privileges of the API's database role |

Run the tests:

```bash
make test-unit                 # go test ./internal/...
make test                      # go test ./...  (integration tests run only if TEST_DATABASE_URL is set, e.g. via .env)

make db-up                     # PostgreSQL with the trips_test database
make test-integration          # go test -count=1 -v ./tests/...  (uses TEST_DATABASE_URL from .env)

make test-race                 # go test -race -count=1 ./...  (needs cgo + a C compiler)
make test-race-docker          # same, inside a golang:1.26 container on the compose network
make lint                      # gofmt check + go vet
```

> ⚠️ The integration tests **truncate** `trips` and `idempotency_keys`. `TEST_DATABASE_URL` must point at a dedicated database. Compose creates `trips_test` for this purpose on first start.

With `TEST_APP_DATABASE_URL` set (as in `.env.example` and `make test-race-docker`), the API under test connects as `trip_api`, so the suite proves the restricted role is sufficient. The owner connection is used only for migrations, truncation and assertions.

`make test-race-docker` exists because the race detector needs cgo. It gives the same `-race` coverage on machines without a C toolchain, such as Windows without MinGW.

## OpenAPI documentation

The contract is in [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.0.3). It is embedded in the binary and served by the running API:

```bash
curl http://localhost:8080/openapi.yaml
```

To browse it interactively, paste the file into [editor.swagger.io](https://editor.swagger.io), or run Swagger UI locally:

```bash
docker run --rm -p 8081:8080 -e SWAGGER_JSON=/spec/openapi.yaml -v "$PWD/api:/spec" swaggerapi/swagger-ui
# then open http://localhost:8081
```

---

## Security and production readiness

**In place:**

- **SQL injection:** every query is a fixed string with bind parameters. Even the status filter selects between two constant statements rather than building SQL.
- **Authentication:** every `/trips` route requires a per-client API key.
  - Keys have 256 bits of entropy and are stored only as SHA-256 hashes, so a database leak reveals no usable key.
  - Revocation is effective on the next request.
  - Malformed keys are rejected before any database lookup.
  - Access logs record the `client_id`, never the key.
- **Least-privilege database access:** the API connects as `trip_api`, which can only read and write rows; see [Database roles](#database-roles).
- **Information leakage:** errors are sanitized to stable codes. Panics return a generic `500`, and their stack traces go to logs only.
- **Abuse limits:** request body cap (`413`), `MaxHeaderBytes` of 16 KiB, read-header, read, write and idle timeouts, a per-request context deadline, and a bounded DB pool.
- **Log hygiene:** access logs record method, path, route, status, size and duration. They never record bodies, headers or query strings, so no personal data or secrets end up in logs. `DATABASE_URL` parse errors are not echoed, because they could contain the password. Inbound `X-Request-ID` values are restricted to `[A-Za-z0-9._-]{1,128}` to prevent header and log injection.
- **Container:** distroless, non-root, static binary, no shell. Credentials come only from the environment; nothing is baked into the image.
- **Health:** `/healthz` (liveness) never touches the database, so a DB outage does not trigger restart loops. `/readyz` checks the database and returns `503` while draining. Startup fails loudly on bad config, an unreachable DB (after `DB_CONNECT_TIMEOUT`), or an outdated schema.
- **Graceful shutdown:** on SIGTERM:
  1. Readiness turns to `503`.
  2. The server keeps serving for `SHUTDOWN_DRAIN_DELAY`, so load balancers can stop routing to it.
  3. In-flight requests finish within `SHUTDOWN_TIMEOUT`.
  4. The pool closes.

  A second signal exits immediately.
- **Dependency outages:** the following all map to a retryable `503 SERVICE_UNAVAILABLE`, and the service recovers automatically:
  - connection failures;
  - connections lost mid-query (restart, failover, dropped socket);
  - lock and statement timeouts.

  This was verified by stopping PostgreSQL under a running API.

**Required before real production use:**

- Authorization: per-client permissions or tenant isolation, if clients should not all have full access.
- Rate limiting.
- TLS to the database (`sslmode=verify-full`); the `sslmode=disable` in `.env.example` is for local use only.
- Secrets from a secret manager.
- Metrics and tracing (OpenTelemetry).
- A scheduled purge of expired idempotency keys.

## Design decisions and trade-offs

| Decision | Why | Trade-off |
|---|---|---|
| Pessimistic `SELECT ... FOR UPDATE` | Directly expresses "validate against the current persisted status". Easy to reason about and test. | Holds a row lock for one short transaction. A conditional `UPDATE ... WHERE status = ANY($allowed_from)` would do one round trip, but makes it harder to tell `404` from `409` and to keep transition logic in Go. |
| Transition rules in Go, statuses also in a `CHECK` | The state machine stays readable and unit-testable; the DB still guards the value domain. | Transitions themselves are not DB-enforced (that would need a trigger). |
| UUIDv7 IDs generated in Go | Time-ordered, so B-tree inserts stay local. Generated consistently in one place. | Reveals approximate creation time. |
| Timestamps from PostgreSQL `now()` | One clock (the DB), so responses equal stored values exactly. | Slightly less control in tests. |
| Offset pagination | Matches the required `page`/`page_size` contract. | Deep pages get slower (`page` is capped at 1,000,000); keyset pagination scales better. |
| `400` for all input errors, no `422` | One rule for clients. | Less granular than some APIs. |
| Trim whitespace | `" T-1 "` and `"T-1"` should not be two trips. | Values are not stored byte-for-byte as sent. |
| Idempotency snapshot replay | The retried request gets exactly the original response. | Snapshot can be stale relative to the current trip. That is intended, and documented. |
| Separate migrate step | Safe with many replicas; failures are visible. | One more deployment step (automated in Compose). |
| Physical delete, any status | Matches the brief, simple semantics. | No audit trail yet; see [Audit history (planned)](#audit-history-planned). |
| API keys rather than JWT | Fits service-to-service clients; needs no identity provider; revocation is immediate. | One indexed lookup per request, and key distribution is manual (`apikey create`). |
| SHA-256 for key hashes, not bcrypt | Keys are 256-bit random, so there is nothing to brute-force, and a slow hash on every request would only add latency. | Not suitable for human passwords (none are stored). |
| Two small interfaces (`handler.TripService`, `auth.ClientStore`) | Handler and middleware tests need fakes. Nothing else does. | The service is tested via integration tests rather than mocks. |

## Audit history (planned)

> **Current status:** audit history is a planned improvement. It is **not implemented** in the current version.

Because of the assignment's time constraints, the current implementation prioritizes core trip management and safe status transitions. A trip stores only its current `status` and `updated_at`, so earlier states, and who changed them, are not recorded.

For a production logistics system, a dedicated `trip_status_history` table would improve traceability and debugging:

| Column | Records |
|---|---|
| `trip_id` | The trip that changed |
| `previous_status` | Status before the change |
| `new_status` | Status after the change |
| `changed_at` | Timestamp of the change |
| `changed_by` | The user or system responsible, such as the authenticated API client's `client_id`, which every request already carries |

**Consistency:** the status update and its history record must be written in the same database transaction. In this codebase that means inserting the history row inside `TripRepository.UpdateStatus`, after the `UPDATE` and before `COMMIT`, while the row lock is still held. That gives two guarantees:

- Either both the change and its record are saved, or neither is.
- Concurrent updates are already serialized by the lock, so history order matches the order in which changes were applied.

How it would fit the existing design:

- **Privileges.** Following [Database roles](#database-roles), its migration would grant `trip_api` only `INSERT` and `SELECT` on the table. The API could then append history but never rewrite it.
- **Deleted trips.** Trips are physically deleted today, so history rows must not cascade away with their trip. Pairing this with soft deletion (`deleted_at`) would keep deleted trips reviewable.

This enhancement can be implemented in a future iteration, alongside trip history retrieval (for example `GET /trips/{id}/history`) and audit reporting.

## Known limitations and future work

- **Authentication without authorization.** Every valid key has full access to all trips. There are no per-client permissions, scopes or tenant isolation.
- **One database lookup per authenticated request** (a unique-index hit) keeps revocation immediate. A short in-process cache would remove it, at the cost of a revocation delay.
- **Expired idempotency keys are reclaimed on reuse but not purged.** Add a scheduled `DELETE FROM idempotency_keys WHERE expires_at < now()` (indexed).
- **`total_items` uses `count(*)`,** which is linear in the number of matching rows. For very large tables, consider estimates or keyset pagination with "has more".
- **No audit history of status changes.** This is planned; see [Audit history (planned)](#audit-history-planned).
- **No optimistic concurrency for clients** (`ETag` / `If-Match`) to detect lost updates across read-modify-write cycles on the client side.
- **No metrics or tracing endpoints.**
- **The integration tests share one database** and are not run in parallel with each other.
