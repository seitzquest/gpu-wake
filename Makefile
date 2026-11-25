# gpu-wake: build, test and package. The proxy is a static Go binary; the host side is shell.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/arm64 linux/armv7 linux/amd64
SHELL_SCRIPTS := host/bin/* host/install.sh host/install-user.sh host/sleep-hook host/test/*.sh \
	proxy/install.sh proxy/gpu-bridge scripts/e2e-test.sh examples/checks.d/*

.PHONY: build test lint dist clean

build:
	cd proxy && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o gpu-wake-proxy .

test:
	cd proxy && go vet ./... && go test -race ./...
	host/test/idle_test.sh

lint:
	test -z "$$(gofmt -l proxy)" || { gofmt -l proxy; exit 1; }
	shellcheck $(SHELL_SCRIPTS)

# Release archives: the binary for each platform plus the host scripts, units, docs and examples.
dist: clean
	mkdir -p dist
	set -e; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; goarch=$$arch; goarm=; \
	  if [ $$arch = armv7 ]; then goarch=arm; goarm=7; fi; \
	  name=gpu-wake-$(VERSION)-$$os-$$arch; \
	  mkdir -p dist/$$name/proxy; \
	  (cd proxy && CGO_ENABLED=0 GOOS=$$os GOARCH=$$goarch GOARM=$$goarm \
	    go build -trimpath -ldflags "$(LDFLAGS)" -o ../dist/$$name/proxy/gpu-wake-proxy .); \
	  cp -r host examples docs README.md LICENSE CHANGELOG.md dist/$$name/; \
	  cp proxy/install.sh proxy/gpu-bridge proxy/gpu-wake-proxy.service proxy/proxy.json.example \
	    dist/$$name/proxy/; \
	  tar -C dist -czf dist/$$name.tar.gz $$name; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

clean:
	rm -rf dist proxy/gpu-wake-proxy
