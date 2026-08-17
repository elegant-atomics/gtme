BINARY := bin/gtme
PKG    := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

.PHONY: check fmt vet test build install clean tidy live live-deliver

check: fmt vet test

fmt:
	@out=$$(gofmt -l $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './bin/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet $(PKG)

test:
	go test $(PKG)

build:
	go build -ldflags "-X github.com/elegant-atomics/gtme/internal/cli.Version=$(VERSION)" -o $(BINARY) ./cmd/gtme

# install puts `gtme` on your PATH (~/.local/bin by default; see install.sh
# for PREFIX). After this, every `./bin/gtme` in the docs is just `gtme`.
install:
	./install.sh

# live runs the manual provider smoke tests (SPEC §12: a human gate). Each test
# skips unless its credential is set; nothing is delivered to a real campaign
# unless GTME_LIVE_DELIVER=yes.
live:
	go test -tags live -count=1 -v ./test/live/

live-deliver:
	GTME_LIVE_DELIVER=yes go test -tags live -count=1 -v -run Deliver ./test/live/

tidy:
	go mod tidy

clean:
	rm -rf bin
