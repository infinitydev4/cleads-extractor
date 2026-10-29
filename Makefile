GO        ?= go
VERSION   ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
TEST_DSN  ?= postgres://cleads:cleads@localhost:5432/cleads?sslmode=disable

.PHONY: build run test test-pg race bench bench-e2e lint docker up down

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/cleads-extractor ./cmd/server
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/cleads-mcp ./cmd/mcp
	$(GO) build -o bin/bench ./cmd/bench

run:
	APP_ENV=development AUTH_DISABLED=true LOG_FORMAT=text ENABLE_MOCK_SOURCE=true \
	ENABLE_GMAPS_SCRAPING=false ENABLE_OSM=false ENABLE_SIRENE=false ENRICH_ENABLED=false GEOCODER=none $(GO) run ./cmd/server

test:
	$(GO) vet ./...
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

# Integration tests against a disposable database (tables are truncated!).
test-pg:
	TEST_DATABASE_URL=$(TEST_DSN) $(GO) test -race -count=1 ./internal/store/pgstore/

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/...

bench-e2e: build
	./bin/bench -store pg -dsn $(TEST_DSN) -workers 1 -area-km 20 -concurrency 64
	./bin/bench -store pg -dsn $(TEST_DSN) -workers 4 -area-km 20 -concurrency 64

docker:
	docker build --build-arg VERSION=$(VERSION) -t cleads-extractor:$(VERSION) .

up:
	docker compose -f deploy/docker-compose.yml up --build -d --scale worker=2

down:
	docker compose -f deploy/docker-compose.yml down
