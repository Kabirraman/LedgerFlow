VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: help backend frontend build test lint typecheck docker-up docker-down

help:
	@echo "make backend      # run the Go API on :8080"
	@echo "make frontend     # run the Next.js dev server on :3000"
	@echo "make build        # build the backend binary into backend/bin/"
	@echo "make test         # run backend Go tests"
	@echo "make lint         # run frontend lint"
	@echo "make typecheck    # run frontend tsc --noEmit"
	@echo "make docker-up    # docker compose up --build"
	@echo "make docker-down  # docker compose down"

backend:
	cd backend && go run ./cmd/server

frontend:
	cd frontend && npm run dev

build:
	cd backend && go build -ldflags="-s -w -X main.buildVersion=$(VERSION)" -o bin/ledgerflow ./cmd/server

test:
	cd backend && go test ./...

lint:
	cd frontend && npm run lint

typecheck:
	cd frontend && npm run typecheck

docker-up:
	docker compose up --build

docker-down:
	docker compose down
