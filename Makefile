.PHONY: up down migrate run-api run-worker test tidy build fmt

up:
	docker-compose up -d postgres redis clickhouse

down:
	docker-compose down

migrate:
	@echo "TODO: wire up goose or plain psql migration runner"

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

test:
	go test ./...

tidy:
	go mod tidy

build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker

fmt:
	go fmt ./...
