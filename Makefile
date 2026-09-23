# Shortcuts; `make up` is the one-command launch. Go targets run from backend/,
# where the router picks up backend/.env.
.PHONY: run test up down eval

run:
	cd backend && go run ./cmd/router

test:
	cd backend && go test ./...

up:
	docker compose up --build

down:
	docker compose down

# Paid OpenAI calls. Extra flags via ARGS, e.g. make eval ARGS="-limit 5".
eval:
	cd backend && go run ./cmd/evaluate $(ARGS)
