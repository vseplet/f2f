.PHONY: run build kill dev camp-run camp-deploy camp-logs \
        desktop-dev desktop-build desktop-open desktop-install-wails help

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
	@echo "  make run                  run mac client (sudo, web UI on 127.0.0.1:2202)"
	@echo "  make dev                  run helper (cross-platform: works inside a linux VM too)"
	@echo "  make build                build release binary at ./f2f-mac"
	@echo "  make kill                 kill any running f2f-mac process"
	@echo "  make camp-run             run camp server locally with bun"
	@echo "  make camp-deploy          deploy camp to fly.io"
	@echo "  make camp-logs            tail fly.io logs for camp"
	@echo "  make desktop-dev          run f2f-desktop with hot-reload"
	@echo "  make desktop-build        build f2f-desktop.app to source/desktop/build/bin"
	@echo "  make desktop-open         build + open f2f-desktop.app"
	@echo "  make desktop-install-wails  go install wails CLI (one-time)"

run:
	-$(SUDO) F2F_DEV_ASSETS=$(CURDIR)/source/mac/internal/web/assets go run ./source/mac

dev:
	-$(SUDO) F2F_DEV_ASSETS=$(CURDIR)/source/helper/ui/web/assets go run ./source/helper $(ARGS)

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

desktop-install-wails:
	@# pin to master: v2.12.0 (the latest tag) has a bug in its
	@# package-loader that breaks our build with anacrolix imports.
	go install github.com/wailsapp/wails/v2/cmd/wails@master

desktop-dev:
	cd source/desktop && $(WAILS) dev

desktop-build:
	cd source/desktop && $(WAILS) build

desktop-open: desktop-build
	open source/desktop/build/bin/f2f-desktop.app
