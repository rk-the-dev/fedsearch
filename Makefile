DC := docker compose -f deploy/docker-compose.yml

.PHONY: gen up down reset verify test logs

gen:            ## generate tiered telemetry into ./out
	go run ./cmd/datagen

up:             ## start MinIO, OpenSearch, Postgres and load the data
	$(DC) up -d minio opensearch postgres
	$(DC) up minio-init opensearch-init

down:           ## stop the stack (keeps volumes)
	$(DC) down

reset:          ## stop the stack and delete all data volumes
	$(DC) down -v

verify:         ## Day 1 acceptance checks
	sh scripts/verify.sh

test:
	go test ./...

logs:
	$(DC) logs -f --tail=50
