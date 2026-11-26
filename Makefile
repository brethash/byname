# byname — household short links. See README.md and docs/operations.md.

VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
# ssh target for deploy: you@me (Pi) or admin@me (cloud). PI= still works.
PI      ?= me
TARGET  ?= $(PI)
GO      ?= go
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
FUZZTIME ?= 30s

.PHONY: help gate test race fuzz vet lint vuln shellcheck build build-linux build-arm64 build-amd64 run deploy cloud-check host-test clean

help:
	@echo "make gate         vet + staticcheck + tests (race) + govulncheck + shellcheck"
	@echo "make fuzz         run each fuzz target for FUZZTIME (default 30s)"
	@echo "make run          run locally on http://localhost:8080 (dev mode)"
	@echo "make build-linux  static Linux binaries in bin/byname-linux-{arm64,amd64}"
	@echo "make deploy TARGET=you@me   gate, build, ship to the host, health-check, auto-rollback"
	@echo "make cloud-check  tofu fmt + validate for deploy/cloud/{aws,hetzner}"
	@echo "make host-test    run deploy/host and deploy/systemd on Debian 13 in a container (Docker)"

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
	shellcheck -x test/host/*.sh test/host/stubs/* test/host/stubs-tailscale/*

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
	bin/byname serve --dev --listen 127.0.0.1:8080 --hosts localhost:8080 \
		--allow-cidrs 127.0.0.0/8,::1/128 --no-whois \
		--db tmp/dev.db --snapshot-dir tmp/snapshots --throttled-path "" --log-level debug

deploy: gate build-linux
	deploy/deploy.sh $(TARGET) $(VERSION)

# Needs OpenTofu; `init` downloads the providers but touches no account, and
# the tests use mock providers.
cloud-check:
	tofu fmt -check -recursive deploy/cloud
	for d in aws hetzner; do \
		tofu -chdir=deploy/cloud/$$d init -backend=false -input=false >/dev/null && \
		tofu -chdir=deploy/cloud/$$d validate && \
		tofu -chdir=deploy/cloud/$$d test || exit 1; \
	done

# Debian 13 smoke test of deploy/host and deploy/systemd: bootstrap.sh for every
# platform and access combination (each rendered firewall through nft -c),
# install.sh's deploy and rollback, restore.sh and byname-verify, against stubs
# for systemd, Tailscale, apt and the byname binary. It runs in a throwaway
# container with no network and the repo mounted read-only; nothing outside
# the container changes. NET_ADMIN is for nft inside the container's own
# network namespace. CONTAINER=podman works with rootful Podman.
CONTAINER ?= docker
HOST_TEST_IMAGE ?= byname-host-test
host-test:
	$(CONTAINER) build -q -t $(HOST_TEST_IMAGE) test/host >/dev/null
	$(CONTAINER) run --rm --init --network none --cap-add NET_ADMIN \
		-v "$(CURDIR):/src:ro" $(HOST_TEST_IMAGE) bash /src/test/host/run.sh

clean:
	rm -rf bin tmp
