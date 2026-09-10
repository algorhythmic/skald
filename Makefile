# Prefer an optional project-local toolchain, then Go from PATH. GO remains
# overridable through the environment or `make GO=/path/to/go`.
GO ?= $(if $(wildcard $(CURDIR)/.tools/go/bin/go),$(CURDIR)/.tools/go/bin/go,go)
export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod
BUILD_FLAGS ?= -buildvcs=false

.PHONY: check-go test check build smoke tui-smoke discovery-smoke demo

check-go:
	@command -v "$(GO)" >/dev/null 2>&1 || { \
		printf '%s\n' 'Go was not found. Install Go on PATH, place it in .tools/go, or use make GO=/absolute/path/to/go.' >&2; \
		exit 1; \
	}

test: check-go
	$(GO) test ./...
	python3 scripts/check_schema.py

check: test
	$(GO) vet ./...

build: check-go
	$(GO) build $(BUILD_FLAGS) -o bin/skald ./cmd/skald

smoke: build
	python3 scripts/native_smoke.py

tui-smoke: build
	python3 scripts/tui_smoke.py

demo: build
	python3 scripts/tui_demo.py

discovery-smoke: build
	python3 scripts/discovery_smoke.py
