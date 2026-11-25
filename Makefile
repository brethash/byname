# byname — household short links. See README.md and docs/operations.md.

VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
# ssh target for deploy: you@nym (Pi) or admin@nym (cloud). PI= still works.
PI      ?= nym
TARGET  ?= $(PI)
GO      ?= go
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
FUZZTIME ?= 30s

.PHONY: help gate test race fuzz vet lint vuln shellcheck build build-linux build-arm64 build-amd64 run deploy cloud-check clean

help:
	@echo "make gate         vet + staticcheck + tests (race) + govulncheck + shellcheck"
	@echo "make fuzz         run each fuzz target for FUZZTIME (default 30s)"
	@echo "make run          run locally on http://localhost:8080 (dev mode)"
	@echo "make build-linux  static Linux binaries in bin/byname-linux-{arm64,amd64}"
	@echo "make deploy TARGET=you@nym   gate, build, ship to the host, health-check, auto-rollback"
	@echo "make cloud-check  tofu fmt + validate for deploy/cloud/{aws,hetzner}"

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
	shellcheck -x deploy/deploy.sh deploy/host/*.sh deploy/cloud/first-boot.sh

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/byname ./cmd/byname

# Raspberry Pi, AWS Lightsail and Hetzner servers are arm64 or amd64; deploy.sh
# asks the host which one it is.
build-linux: build-arm64 build-amd64

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/byname-linux-arm64 ./cmd/byname

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/byname-linux-amd64 ./cmd/byname

run: build
	@mkdir -p tmp
	bin/byname serve --listen 127.0.0.1:8080 --hosts localhost:8080 \
		--allow-cidrs 127.0.0.0/8,::1/128 --no-whois \
		--db tmp/dev.db --snapshot-dir tmp/snapshots --throttled-path "" --log-level debug

deploy: gate build-linux
	deploy/deploy.sh $(TARGET) $(VERSION)

# Needs OpenTofu; `init` downloads the providers but touches no account.
cloud-check:
	tofu fmt -check -recursive deploy/cloud
	for d in aws hetzner; do \
		tofu -chdir=deploy/cloud/$$d init -backend=false -input=false >/dev/null && \
		tofu -chdir=deploy/cloud/$$d validate || exit 1; \
	done

clean:
	rm -rf bin tmp
