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

.PHONY: build test vet fmt cross clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nfrs ./cmd/nfrs
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nfrc ./cmd/nfrc

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Statically linked binaries, no CGO: 6 platforms x 2 binaries = 12 assets.
cross:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-linux-amd64      ./cmd/nfrs
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-linux-amd64      ./cmd/nfrc
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-linux-arm64      ./cmd/nfrs
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-linux-arm64      ./cmd/nfrc
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm   go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-linux-armv7      ./cmd/nfrs
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm   go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-linux-armv7      ./cmd/nfrc
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-darwin-arm64     ./cmd/nfrs
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-darwin-arm64     ./cmd/nfrc
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-darwin-amd64     ./cmd/nfrs
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-darwin-amd64     ./cmd/nfrc
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrs-windows-amd64.exe ./cmd/nfrs
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nfrc-windows-amd64.exe ./cmd/nfrc

clean:
	rm -rf bin dist
