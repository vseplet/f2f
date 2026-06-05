.PHONY: build kill dev camp-run camp-deploy camp-logs help

# wails CLI lives under $GOPATH/bin — use it directly so a stale PATH
# doesn't make `wails` "not found" while it's actually installed.
WAILS ?= $(shell go env GOPATH)/bin/wails

# SUDO is empty when we're already root, otherwise "sudo". Prevents
# nested sudo when a user types `sudo make dev` — that nesting
# overwrites SUDO_USER with "root" and the helper writes config under
# /var/root/.f2f/ instead of the real user's home.
SUDO := $(if $(filter 0,$(shell id -u)),,sudo)

help:
	@echo "f2f targets:"
	@echo "  make dev                  run helper (cross-platform: works inside a linux VM too)"
	@echo "  make build                build release binary at ./f2f-mac"
	@echo "  make kill                 kill any running f2f-mac process"
	@echo "  make camp-run             run camp server locally (go run)"
	@echo "  make camp-deploy          deploy camp to fly.io"
	@echo "  make camp-logs            tail fly.io logs for camp"
dev:
	-$(SUDO) F2F_DEV_ASSETS=$(CURDIR)/source/helper/ui/web/assets go run ./source/helper --console $(ARGS)

build:
	go build -o f2f-mac ./source/mac
	@echo "built: $$(pwd)/f2f-mac"

kill:
	-sudo pkill -f f2f-mac

camp-run:
	cd source/camp && go run .

# Build context must be source/ so the Docker build can see the helper
# module (camp imports its wire types). The config lives in camp/, and
# fly resolves `dockerfile` relative to the config dir.
camp-deploy:
	flyctl deploy source --config camp/fly.toml -a f2f-camp

camp-logs:
	cd source/camp && flyctl logs
