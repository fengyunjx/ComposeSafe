VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X composesafe/internal/cli.Version=$(VERSION)

.PHONY: build test vet check integration install clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/composesafe ./cmd/composesafe
	ln -sf composesafe bin/csafe

test:
	go test ./...

vet:
	go vet ./...

check: vet test
	test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt needed" >&2; exit 1)
	sh scripts/check-no-delete.sh

# Needs a running Docker daemon; creates and removes throwaway projects named cstest*.
# CS_TEST_HELPER overrides the helper image (default alpine:3).
integration:
	go test -tags integration ./test/integration -count=1 -timeout 20m

install: build
	install -m 0755 bin/composesafe "$${GOBIN:-$$(go env GOPATH)/bin}/composesafe"
	ln -sf composesafe "$${GOBIN:-$$(go env GOPATH)/bin}/csafe"

clean:
	rm -rf bin
