# ot — household short links. See README.md and docs/operations.md.

VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
PI      ?= ot
GO      ?= go
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
FUZZTIME ?= 30s

.PHONY: help gate test race fuzz vet lint vuln shellcheck build build-arm64 run deploy clean

help:
	@echo "make gate         vet + staticcheck + tests (race) + govulncheck + shellcheck"
	@echo "make fuzz         run each fuzz target for FUZZTIME (default 30s)"
	@echo "make run          run locally on http://localhost:8080 (dev mode)"
	@echo "make build-arm64  static binary for the Pi in bin/ot-linux-arm64"
	@echo "make deploy PI=you@ot   gate, build, ship to the Pi, health-check, auto-rollback"

gate: vet lint race vuln shellcheck

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

fuzz:
	$(GO) test ./internal/link -run '^$$' -fuzz FuzzValidateTarget -fuzztime $(FUZZTIME)
	$(GO) test ./internal/link -run '^$$' -fuzz FuzzExpand -fuzztime $(FUZZTIME)

vet:
	$(GO) vet ./...

lint:
	$(GO) tool staticcheck ./...

vuln:
	$(GO) tool govulncheck ./...

shellcheck:
	shellcheck -x deploy/deploy.sh deploy/pi/*.sh

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/ot ./cmd/ot

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ot-linux-arm64 ./cmd/ot

run: build
	@mkdir -p tmp
	bin/ot serve --listen 127.0.0.1:8080 --hosts localhost:8080 \
		--allow-cidrs 127.0.0.0/8,::1/128 --no-whois \
		--db tmp/dev.db --snapshot-dir tmp/snapshots --throttled-path "" --log-level debug

deploy: gate build-arm64
	deploy/deploy.sh $(PI) $(VERSION)

clean:
	rm -rf bin tmp
