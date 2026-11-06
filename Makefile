VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X composesafe/internal/cli.Version=$(VERSION)

STATICCHECK_VERSION ?= 2025.1.1
GOOS_ARCHS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build test vet lint check integration install dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/composesafe ./cmd/composesafe
	ln -sf composesafe bin/csafe

test:
	go test ./...

vet:
	go vet ./...

# staticcheck is not part of `check` so that a fresh checkout needs nothing but Go;
# CI runs it. Install: go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
lint:
	staticcheck ./...
	staticcheck -tags integration ./test/...

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

# Static binaries for a release: dist/composesafe_<version>_<os>_<arch>.tar.gz plus SHA256SUMS.
# Run `make clean` first to start from an empty dist/.
dist:
	mkdir -p dist
	set -e; for t in $(GOOS_ARCHS); do \
	  os=$${t%/*}; arch=$${t#*/}; name=composesafe_$(VERSION)_$${os}_$${arch}; \
	  mkdir -p dist/$$name; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS) -s -w" -o dist/$$name/composesafe ./cmd/composesafe; \
	  ln -sf composesafe dist/$$name/csafe; \
	  cp README.md dist/$$name/; \
	  tar -C dist -czf dist/$$name.tar.gz $$name; \
	done
	cd dist && (sha256sum *.tar.gz 2>/dev/null || shasum -a 256 *.tar.gz) > SHA256SUMS

clean:
	rm -rf bin dist
