MODULE  := github.com/tiennm99/MTClaw
BIN     := mtclaw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X '$(MODULE)/internal/version.Version=$(VERSION)' \
           -X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
           -X '$(MODULE)/internal/version.Date=$(DATE)'

.PHONY: build test race lint fmt install clean release

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) .

test:
	CGO_ENABLED=0 go test ./...

# -race requires cgo (a real C compiler), independent of the CGO-free
# release build above - see .github/workflows/ci.yml's own comment on this.
race:
	go test -race ./...

lint:
	go vet ./...

fmt:
	gofmt -l .

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" .

clean:
	rm -rf bin/ dist/

# release cross-compiles the exact matrix release.yml builds, stamps the
# same -X paths, and writes SHA256SUMS - so a maintainer can produce and
# spot-check the release artifacts locally before ever pushing a tag.
RELEASE_LDFLAGS := -s -w $(LDFLAGS)
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

release: clean
	mkdir -p dist
	@for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="dist/$(BIN)-$(VERSION)-$$os-$$arch$$ext"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o "$$out" . || exit 1; \
	done
	cd dist && sha256sum * > SHA256SUMS
