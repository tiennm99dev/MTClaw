MODULE  := github.com/tiennm99/MTClaw
BIN     := mtclaw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Only the tag name needs an explicit -X: internal/version.String resolves
# Commit and Date on its own from runtime/debug.ReadBuildInfo's VCS
# stamping (the commit SHA and commit time, not build wall-clock time),
# which Go embeds automatically for any binary built from within this git
# checkout - see internal/version/version.go.
LDFLAGS := -X '$(MODULE)/internal/version.Version=$(VERSION)'

.PHONY: build test race lint fmt fmt-check install clean release

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) .

test:
	CGO_ENABLED=0 go test ./...

# -race requires cgo (a real C compiler), independent of the CGO-free
# release build above - see .github/workflows/ci.yml's own comment on this.
race:
	go test -race ./...

# golangci-lint's linter set and exclusions live in .golangci.yml; CI runs
# the same command on its ubuntu leg. Install it with
# `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
lint:
	go vet ./...
	golangci-lint run ./...

# fmt rewrites every unformatted file in place; use fmt-check (the same gofmt -l
# check CI inlines, not a call to this target) to only report a problem and
# fail without touching anything.
fmt:
	gofmt -w .

fmt-check:
	@fmtOut="$$(gofmt -l .)"; \
	if [ -n "$$fmtOut" ]; then \
		echo "gofmt found unformatted files:"; \
		echo "$$fmtOut"; \
		exit 1; \
	fi

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" .

clean:
	rm -rf bin/ dist/

# release cross-compiles the exact matrix release.yml builds, stamps the
# same -X path, and writes SHA256SUMS - so a maintainer can produce and
# spot-check the release artifacts locally before ever pushing a tag, and so
# release.yml itself has exactly one place that knows the build recipe.
# VERSION is normally overridden on the command line by the release
# workflow (`make release VERSION=$GITHUB_REF_NAME`); the git-describe
# default above is what a local `make release` gets instead.
RELEASE_LDFLAGS := -s -w $(LDFLAGS)
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# sha256sum is not available on macOS by default (it ships `shasum -a 256`
# instead); checksum picks whichever this host actually has.
checksum := $(shell command -v sha256sum 2>/dev/null || echo "shasum -a 256")

release: clean
	mkdir -p dist
	@for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="dist/$(BIN)-$(VERSION)-$$os-$$arch$$ext"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o "$$out" . || exit 1; \
	done
	cd dist && $(checksum) * > SHA256SUMS
