MODULE  := github.com/solitudeit/nfr
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Two binaries: nfrs runs on the public server, nfrc runs inside the network.
PLATFORMS := linux/amd64 linux/arm64 linux/armv7 darwin/arm64 darwin/amd64 windows/amd64

.PHONY: web clients build test vet fmt cross release clean

web:
	@if [ ! -d web/node_modules ]; then pnpm --dir web install --frozen-lockfile; fi
	pnpm --dir web build

clients:
	python3 scripts/build_clients.py --version "$(VERSION)" --out bin/clients

build: web
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nfrs ./cmd/nfrs
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nfrc ./cmd/nfrc

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Build self-contained release archives for Windows, Linux, macOS, and Android.
cross release: web
	python3 scripts/package.py --version "$(VERSION)" --out dist

clean:
	rm -rf bin dist
