# Relay: build, test and release helpers.  `make help` lists the targets.
MODULE  := github.com/thesahibnanda-max/relay
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/cli.Version=$(VERSION)
GO      ?= go
FUZZTIME ?= 10s

.PHONY: help build install test race short vet vet-all fmt fmt-check fuzz cross check notices clean

help:            ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | column -t -s "$$(printf '\t')"

build:           ## build ./bin/relay
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/relay .

install:         ## install relay into $GOBIN / $GOPATH/bin
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" .

test:            ## all tests
	$(GO) test ./...

race:            ## all tests under the race detector (what CI runs)
	$(GO) test -race -count=1 ./...

short:           ## quick tests: skips the binary end-to-end, chaos and soak tests
	$(GO) test -short ./...

vet:             ## go vet
	$(GO) vet ./...

# Editors (gopls) analyse every platform and flag code that does not compile
# there, even though relay only RUNS on Linux, macOS and WSL. Keep them all clean.
vet-all:         ## go vet for linux, macOS and Windows (what the editor checks)
	@for t in linux/amd64 darwin/arm64 windows/amd64; do \
	  echo "vet $$t"; GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) vet ./... || exit 1; \
	done

fmt:             ## format the code
	gofmt -w .

fmt-check:       ## fail if anything is unformatted
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

fuzz:            ## run every fuzz target briefly (FUZZTIME=30s to lengthen)
	$(GO) test ./internal/agent     -run XXX -fuzz '^FuzzInputParser$$'     -fuzztime $(FUZZTIME)
	$(GO) test ./internal/agent     -run XXX -fuzz '^FuzzBuildInjection$$'  -fuzztime $(FUZZTIME)
	$(GO) test ./internal/proto     -run XXX -fuzz '^FuzzEnvelope$$'        -fuzztime $(FUZZTIME)
	$(GO) test ./internal/transcript -run XXX -fuzz '^FuzzClaudeParser$$'   -fuzztime $(FUZZTIME)
	$(GO) test ./internal/transcript -run XXX -fuzz '^FuzzCodexParser$$'    -fuzztime $(FUZZTIME)
	$(GO) test ./internal/redact    -run XXX -fuzz '^FuzzRedact$$'          -fuzztime $(FUZZTIME)
	$(GO) test ./internal/mcp       -run XXX -fuzz '^FuzzServe$$'           -fuzztime $(FUZZTIME)

cross:           ## build for every supported platform into ./dist
	@mkdir -p dist
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
	  ext=""; [ "$${t%/*}" = "windows" ] && ext=".exe"; \
	  echo "building $$t"; \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/relay-$${t%/*}-$${t#*/}$$ext . || exit 1; \
	done

notices:         ## regenerate THIRD_PARTY_NOTICES.md from the linked modules
	./scripts/notices.sh

check: fmt-check vet vet-all race   ## everything CI checks

clean:           ## remove build output
	rm -rf bin dist
