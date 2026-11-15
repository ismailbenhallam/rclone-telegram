BIN     := rclone-telegram
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# macOS needs cgo + the cmount tag + macFUSE/FUSE-T headers for `mount`.
# It is enabled automatically when the headers are installed.
# Linux mounts without cgo. Override with: make build TAGS=
ifeq ($(shell uname -s),Darwin)
ifneq ($(wildcard /usr/local/include/fuse/fuse.h /usr/local/include/osxfuse/fuse/fuse.h),)
TAGS ?= cmount
export CGO_ENABLED := 1
endif
endif

.PHONY: build test vet plugin clean release

# Tag the next version (see scripts/next-version.sh) and push it, which
# triggers the release workflow. Run on an up-to-date, clean main.
release: vet test
	@test -z "$$(git status --porcelain)" || (echo "working tree not clean" && exit 1)
	@git fetch --tags --quiet
	@v=$$(./scripts/next-version.sh) && \
	  git tag -a "$$v" -m "Release $$v" && \
	  git push origin "$$v" && \
	  echo "Pushed $$v: release workflow started"

build:
	go build -trimpath -tags "$(TAGS)" -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)

# Experimental: see plugin/main.go for the strict compatibility rules.
plugin:
	CGO_ENABLED=1 go build -buildmode=plugin -o librcloneplugin_telegram.so ./plugin

clean:
	rm -rf $(BIN) dist *.so
