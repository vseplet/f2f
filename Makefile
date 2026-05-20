.PHONY: run build kill camp-run camp-deploy camp-logs help

help:
	@echo "f2f targets:"
	@echo "  make run          run mac client (sudo, web UI on 127.0.0.1:2202)"
	@echo "  make build        build release binary at ./f2f-mac"
	@echo "  make kill         kill any running f2f-mac process"
	@echo "  make camp-run     run camp server locally with bun"
	@echo "  make camp-deploy  deploy camp to fly.io"
	@echo "  make camp-logs    tail fly.io logs for camp"

run:
	-sudo go run ./source/mac

build:
	go build -o f2f-mac ./source/mac
	@echo "built: $$(pwd)/f2f-mac"

kill:
	-sudo pkill -f f2f-mac

camp-run:
	cd source/camp && bun run src/server.ts

camp-deploy:
	cd source/camp && flyctl deploy

camp-logs:
	cd source/camp && flyctl logs
