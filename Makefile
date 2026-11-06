GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
PREFIX   ?= /opt/backup-tool
DESTDIR  ?=

.DEFAULT_GOAL := build
.DELETE_ON_ERROR:
.PHONY: build build-linux-amd64 build-linux-arm64 test vet check release install clean

build:
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -ldflags "$(LDFLAGS)" -o backup-tool .

build-linux-amd64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -mod=readonly -trimpath -ldflags "$(LDFLAGS)" -o dist/backup-tool-linux-amd64 .

build-linux-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -mod=readonly -trimpath -ldflags "$(LDFLAGS)" -o dist/backup-tool-linux-arm64 .

# Only the example configuration is packaged; local credentials are never included.
release: build-linux-amd64 build-linux-arm64
	@set -eu; \
	stage=$$(mktemp -d dist/.package.XXXXXX); \
	trap 'rm -rf "$$stage"' EXIT HUP INT TERM; \
	for arch in amd64 arm64; do \
		install -m 755 "dist/backup-tool-linux-$$arch" "$$stage/backup-tool"; \
		install -m 644 backup-tool.example.toml README.md LICENSE THIRD_PARTY_NOTICES.txt "$$stage/"; \
		tar -czf "dist/backup-tool-linux-$$arch.tar.gz" -C "$$stage" \
			backup-tool backup-tool.example.toml README.md LICENSE THIRD_PARTY_NOTICES.txt; \
	done
	install -m 644 THIRD_PARTY_NOTICES.txt dist/THIRD_PARTY_NOTICES.txt
	cd dist && shasum -a 256 backup-tool-linux-amd64 backup-tool-linux-arm64 \
		backup-tool-linux-amd64.tar.gz backup-tool-linux-arm64.tar.gz THIRD_PARTY_NOTICES.txt > SHA256SUMS

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: vet test

# Run on the target machine. Existing configuration and backup data are preserved.
install: build
	install -d -m 700 "$(DESTDIR)$(PREFIX)"
	install -m 755 backup-tool "$(DESTDIR)$(PREFIX)/backup-tool"
	install -m 644 README.md LICENSE THIRD_PARTY_NOTICES.txt "$(DESTDIR)$(PREFIX)/"
	@if [ ! -e "$(DESTDIR)$(PREFIX)/backup-tool.toml" ] && [ ! -L "$(DESTDIR)$(PREFIX)/backup-tool.toml" ]; then \
		install -m 600 backup-tool.example.toml "$(DESTDIR)$(PREFIX)/backup-tool.toml"; \
	fi

clean:
	rm -f backup-tool
	rm -rf dist
