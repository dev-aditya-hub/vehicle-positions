# Development Guide

This guide is for contributors who want to run, verify, and iterate on the current Go server in this repository.

## Scope

The current implementation focuses on:

- ingesting location reports (`POST /api/v1/locations`)
- serving GTFS-RT vehicle positions (`GET /gtfs-rt/vehicle-positions`)
- exposing basic server status (`GET /api/v1/admin/status`)

## Prerequisites

- Go (matching `go.mod` toolchain)
- Docker + Docker Compose
- `curl`

## Quick Start (Docker)

From the repository root:

1. Start the stack:

   ```bash
   make up
   ```

2. Verify server health:

   ```bash
   curl http://localhost:8080/health
   ```

3. Seed a development driver (`driver@test.com` / `password`):

   ```bash
   docker compose exec -T db psql -U postgres -d vehicle_positions < seed_dev.sql
   ```

4. Log in to get a JWT. Every endpoint below except `/health` and the
   GTFS-RT feed requires one:

   ```bash
   export TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
     -H 'Content-Type: application/json' \
     -d '{"email":"driver@test.com","password":"password"}' | jq -r .token)
   ```

5. Run a smoke test (posts one location, then fetches status + feed JSON):

   ```bash
   TOKEN=$TOKEN make smoke
   ```

   `make smoke` also queries `/api/v1/admin/status`, which requires the
   `admin` role, so this step needs a token from an admin account rather
   than the seeded driver.

6. Stop the stack when done:

   ```bash
   make down
   ```

## Local Server Run (without Docker server container)

You can run Postgres in Docker and run the Go server directly:

1. Start only database:

   ```bash
   docker compose up -d db
   ```

2. Export environment variables:

   ```bash
   export PORT=8080
   export DATABASE_URL='postgres://postgres:postgres@localhost:5432/vehicle_positions?sslmode=disable'
   export STALENESS_THRESHOLD=5m
   # required: the server exits at startup if this is unset or under 32 bytes
   export JWT_SECRET='local-development-only-insecure-secret'
   ```

3. Run server:

   ```bash
   make run
   ```

Migrations are applied automatically on server startup.

## Running Tests

Run all tests:

```bash
make test
```

Notes:

- most tests are unit tests and run without external services
- DB integration tests in `store_test.go` require `DATABASE_URL` and are skipped when it is not set

## Simulating Vehicle Traffic

Use the built-in simulator to generate multiple moving vehicles. It reports to
`POST /api/v1/locations`, so it needs a driver JWT:

```bash
SIM_TOKEN=$TOKEN make simulate
```

Custom example:

```bash
go run ./cmd/simulator -url http://localhost:8080 -vehicles 20 -interval 2s -duration 2m -token "$TOKEN"
```

The server rate limits ingest to one report per five seconds **per driver**,
not per vehicle, so every vehicle sharing a token shares one bucket and the
rest of the reports come back `429`. To drive several vehicles at full rate,
seed one driver per vehicle and pass their tokens as a comma-separated list:

```bash
go run ./cmd/simulator -vehicles 3 -token "$TOKEN_A,$TOKEN_B,$TOKEN_C"
```

The simulator warns at startup when the configured report rate exceeds what
the supplied tokens allow, counts `429`s separately from real failures, and
stops early if the server rejects a token with `401`.

## API Sanity Checks

### Submit one location

```bash
curl -X POST http://localhost:8080/api/v1/locations \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "vehicle_id": "demo-vehicle-42",
    "trip_id": "route-5-0830",
    "latitude": -1.2921,
    "longitude": 36.8219,
    "bearing": 180,
    "speed": 8.5,
    "accuracy": 12,
    "timestamp": '"$(date +%s)"'
  }'
```

### Get feed (JSON debug format)

The feed is the one data endpoint that is still unauthenticated:

```bash
curl 'http://localhost:8080/gtfs-rt/vehicle-positions?format=json'
```

### Get admin status

Requires a token from a user with the `admin` role:

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/admin/status
```

## Troubleshooting

- `connection refused` when posting locations:
  - confirm server is running on `localhost:8080`
- server container exits immediately after `make up`:
  - check `docker compose logs server` for `JWT_SECRET environment variable is not set`
  - the secret must be at least 32 bytes
- `401 invalid token` / `missing or invalid authorization header`:
  - re-run the login step; tokens expire 24 hours after they are issued
  - a token is only valid for the `JWT_SECRET` the server was started with, so
    restarting with a different secret invalidates every existing token
- `403 admin access required`:
  - the endpoint needs a token from a user whose `role` is `admin`; the
    `seed_dev.sql` user is a `driver`
- DB connection/migration errors:
  - check `DATABASE_URL`
  - verify Postgres container is healthy (`docker compose ps`)
- `address already in use` for `0.0.0.0:5432` when running `make up`:
   - another local Postgres is using port `5432`
   - stop that service, or update [docker-compose.yml](docker-compose.yml) to map a different host port and adjust `DATABASE_URL` accordingly
- empty feed:
   - make sure timestamp is within 5 minutes of server time (this is request validation in `handlers.go`, independent of `STALENESS_THRESHOLD`)
  - ensure coordinates are valid and non-zero
