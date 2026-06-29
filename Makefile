.PHONY: dev restart backend frontend frontend-install

dev:
	./scripts/dev.sh

restart:
	./scripts/restart.sh

backend:
	cd backend && go run ./cmd/slack-agent

frontend:
	cd frontend && npm run dev

frontend-install:
	cd frontend && npm install
