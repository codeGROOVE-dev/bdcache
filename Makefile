.PHONY: test lint bench benchmark clean tag release update

# Tag all modules in the repository with a version
# Usage: make tag VERSION=v1.2.3
tag:
	@if [ -z "$(VERSION)" ]; then \
		echo "ERROR: VERSION is required. Usage: make tag VERSION=v1.2.3"; \
		exit 1; \
	fi
	@echo "=== Releasing $(VERSION) ==="
	@echo ""
	@echo "Step 1: Update submodule go.mod files to require sfcache $(VERSION)..."
	@find . -path ./go.mod -prune -o -name go.mod -print | xargs -I{} sed -i '' 's|github.com/codeGROOVE-dev/sfcache v[^ ]*|github.com/codeGROOVE-dev/sfcache $(VERSION)|' {}
	@find . -path ./go.mod -prune -o -name go.mod -print | xargs -I{} sed -i '' 's|github.com/codeGROOVE-dev/sfcache/pkg/persist/localfs v[^ ]*|github.com/codeGROOVE-dev/sfcache/pkg/persist/localfs $(VERSION)|' {}
	@find . -path ./go.mod -prune -o -name go.mod -print | xargs -I{} sed -i '' 's|github.com/codeGROOVE-dev/sfcache/pkg/persist/datastore v[^ ]*|github.com/codeGROOVE-dev/sfcache/pkg/persist/datastore $(VERSION)|' {}
	@echo ""
	@echo "Step 2: Commit go.mod changes..."
	@git add -A
	@git commit -m "Release $(VERSION)" || echo "  (no changes to commit)"
	@echo ""
	@echo "Step 3: Create and push tags..."
	@git tag -a $(VERSION) -m "$(VERSION)" --force
	@git push origin $(VERSION) --force
	@find . -name go.mod -not -path "./go.mod" | while read mod; do \
		dir=$$(dirname $$mod); \
		dir=$${dir#./}; \
		echo "  $$dir/$(VERSION)"; \
		git tag -a $$dir/$(VERSION) -m "$(VERSION)" --force; \
		git push origin $$dir/$(VERSION) --force; \
	done
	@echo ""
	@echo "Step 4: Push commit..."
	@git push origin main
	@echo ""
	@echo "=== Release $(VERSION) complete ==="
	@echo "Tags pushed:"
	@git tag -l "$(VERSION)" "*/$(VERSION)" | sed 's/^/  /'

# Create a GitHub release
# Usage: make release VERSION=v1.2.3
release: update tag
	@echo ""
	@echo "Step 5: Creating GitHub release..."
	@gh release create $(VERSION) --title "$(VERSION)" --notes "Release $(VERSION)"
	@echo ""
	@echo "=== GitHub release $(VERSION) created ==="

test:
	@echo "Running tests in all modules..."
	@find . -name go.mod -execdir go test -v -race -cover -short -run '^Test' ./... \;

lint:
	go vet ./...
	gofmt -s -w .
	go mod tidy

bench:
	go test -bench=. -benchmem

# Run the 5 key benchmarks (~3-5min):
# 1. Single-Threaded Latency
# 2. Zipf Throughput (1 thread)
# 3. Zipf Throughput (16 threads)
# 4. Meta Trace Hit Rate (real-world)
# 5. Zipf Hit Rate (synthetic)
benchmark:
	@echo "=== sfcache Benchmark Suite ==="
	@cd benchmarks && go test -run=TestBenchmarkSuite -v -timeout=300s

clean:
	go clean -testcache

update:
	@echo "Updating dependencies in all modules..."
	@find . -name go.mod -execdir go get -u ./... \;
	@find . -name go.mod -execdir go mod tidy \;

# BEGIN: lint-install .
# http://github.com/codeGROOVE-dev/lint-install

.PHONY: lint
lint: _lint

LINT_ARCH := $(shell uname -m)
LINT_OS := $(shell uname)
LINT_OS_LOWER := $(shell echo $(LINT_OS) | tr '[:upper:]' '[:lower:]')
LINT_ROOT := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

# shellcheck and hadolint lack arm64 native binaries: rely on x86-64 emulation
ifeq ($(LINT_OS),Darwin)
	ifeq ($(LINT_ARCH),arm64)
		LINT_ARCH=x86_64
	endif
endif

LINTERS :=
FIXERS :=

GOLANGCI_LINT_CONFIG := $(LINT_ROOT)/.golangci.yml
GOLANGCI_LINT_VERSION ?= v2.5.0
GOLANGCI_LINT_BIN := $(LINT_ROOT)/out/linters/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(LINT_ARCH)
$(GOLANGCI_LINT_BIN):
	mkdir -p $(LINT_ROOT)/out/linters
	rm -rf $(LINT_ROOT)/out/linters/golangci-lint-*
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LINT_ROOT)/out/linters $(GOLANGCI_LINT_VERSION)
	mv $(LINT_ROOT)/out/linters/golangci-lint $@

LINTERS += golangci-lint-lint
golangci-lint-lint: $(GOLANGCI_LINT_BIN)
	find . -name go.mod -execdir "$(GOLANGCI_LINT_BIN)" run -c "$(GOLANGCI_LINT_CONFIG)" \;

FIXERS += golangci-lint-fix
golangci-lint-fix: $(GOLANGCI_LINT_BIN)
	find . -name go.mod -execdir "$(GOLANGCI_LINT_BIN)" run -c "$(GOLANGCI_LINT_CONFIG)" --fix \;

.PHONY: _lint $(LINTERS)
_lint:
	@exit_code=0; \
	for target in $(LINTERS); do \
		$(MAKE) $$target || exit_code=1; \
	done; \
	exit $$exit_code

.PHONY: fix $(FIXERS)
fix:
	@exit_code=0; \
	for target in $(FIXERS); do \
		$(MAKE) $$target || exit_code=1; \
	done; \
	exit $$exit_code

# END: lint-install .
