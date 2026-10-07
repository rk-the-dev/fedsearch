DC   := docker compose -f deploy/docker-compose.yml
LITE := deploy/lite.json
DOCK := deploy/docker.json
BIN  := bin

.PHONY: help gen build test test-e2e vet bench bench-docker run run-docker demo demo-docker mcp up down reset verify clean

help:           ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | expand -t 16

gen:            ## generate 180 days of tiered telemetry with a known attack into ./out
	go run ./cmd/datagen

build:          ## build all binaries into ./bin
	mkdir -p $(BIN)
	go build -o $(BIN)/fedsearch ./cmd/fedsearch
	go build -o $(BIN)/fedsearch-server ./cmd/server
	go build -o $(BIN)/fedsearch-mcp ./cmd/mcp
	go build -o $(BIN)/fedsearch-demo ./cmd/demo
	go build -o $(BIN)/fedsearch-bench ./cmd/bench
	go build -o $(BIN)/datagen ./cmd/datagen

vet:
	go vet ./...

test:           ## unit + end-to-end tests (e2e needs `make gen`)
	go test ./...

# ---- lite mode: no Docker -------------------------------------------------

run:            ## console + API on :8080, lite mode (Parquet + NDJSON via DuckDB, CSV context)
	go run ./cmd/server -config $(LITE)

demo:           ## five-act terminal demo, lite mode
	go run ./cmd/demo -config $(LITE)

bench:          ## measure every design claim; writes bench/results and docs/benchmarks.md
	go run ./cmd/bench -config $(LITE)

mcp:            ## MCP server on stdio for an agent (lite mode)
	FEDSEARCH_AGENT_KEY=agent-dev-key go run ./cmd/mcp -config $(LITE)

# ---- docker mode: MinIO (S3) + OpenSearch + Postgres -------------------------

up:             ## start the stack and load the generated data
	$(DC) up -d minio opensearch postgres
	$(DC) up minio-init opensearch-init

down:           ## stop the stack (keeps volumes)
	$(DC) down

reset:          ## stop the stack and delete all data volumes
	$(DC) down -v

verify:         ## check the stack's contents against ground truth
	sh scripts/verify.sh

run-docker:     ## console + API against the Docker stack
	go run ./cmd/server -config $(DOCK)

demo-docker:
	go run ./cmd/demo -config $(DOCK)

bench-docker:
	go run ./cmd/bench -config $(DOCK) -md docs/benchmarks-docker.md

clean:
	rm -rf $(BIN) .fedsearch
