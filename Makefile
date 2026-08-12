.PHONY: help generate fmt vet test up down run smoke simulate

help:
	@echo "Available targets:"
	@echo "  make up        - Start local Postgres + server with docker compose"
	@echo "  make down      - Stop local docker compose stack"
	@echo "  make run       - Run server locally (expects DATABASE_URL env var)"
	@echo "  make smoke     - Post sample location and fetch feed/status (needs TOKEN=<admin jwt>)"
	@echo "  make simulate  - Run simulator against local server (needs SIM_TOKEN=<driver jwt>)"
	@echo "  make generate  - Regenerate sqlc code"
	@echo "  make fmt       - Format Go code"
	@echo "  make vet       - Run go vet"
	@echo "  make test      - Run test suite"

generate:
	cd db && sqlc generate

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

up:
	docker compose up --build -d

down:
	docker compose down

run:
	go run .

smoke:
	@test -n "$(TOKEN)" || { \
		echo "TOKEN is required: /api/v1/locations and /api/v1/admin/status both need a JWT."; \
		echo "Use an admin account, since the status check also requires the admin role:"; \
		echo "  TOKEN=\$$(curl -s -X POST http://localhost:8080/api/v1/auth/login \\"; \
		echo "    -H 'Content-Type: application/json' \\"; \
		echo "    -d '{\"email\":\"admin@test.com\",\"password\":\"...\"}' | jq -r .token) make smoke"; \
		exit 1; \
	}
	@echo "Posting sample location..."
	curl --silent --show-error --fail \
		-X POST http://localhost:8080/api/v1/locations \
		-H 'Authorization: Bearer $(TOKEN)' \
		-H 'Content-Type: application/json' \
		-d '{"vehicle_id":"demo-vehicle-1","trip_id":"demo-trip-1","latitude":-1.2864,"longitude":36.8172,"bearing":120,"speed":8.5,"accuracy":5.0,"timestamp":'"$$(date +%s)"'}' >/dev/null
	@echo "OK"
	@echo "Fetching admin status..."
	curl --silent --show-error --fail \
		-H 'Authorization: Bearer $(TOKEN)' \
		http://localhost:8080/api/v1/admin/status | cat
	@echo
	@echo "Fetching GTFS-RT JSON feed..."
	curl --silent --show-error --fail 'http://localhost:8080/gtfs-rt/vehicle-positions?format=json' | cat
	@echo

simulate:
	@test -n "$(SIM_TOKEN)" || { \
		echo "SIM_TOKEN is required: /api/v1/locations needs a driver JWT."; \
		echo "  SIM_TOKEN=\$$(curl -s -X POST http://localhost:8080/api/v1/auth/login \\"; \
		echo "    -H 'Content-Type: application/json' \\"; \
		echo "    -d '{\"email\":\"driver@test.com\",\"password\":\"password\"}' | jq -r .token) make simulate"; \
		echo "Pass a comma-separated list to drive more than one vehicle at full rate."; \
		exit 1; \
	}
	go run ./cmd/simulator -url http://localhost:8080 -vehicles 5 -interval 3s -duration 30s -token '$(SIM_TOKEN)'
