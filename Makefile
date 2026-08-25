# jiku-go
#
# The library and the binary come from the same module, so `make check` covers both.

BINARY  := jiku
PKG     := github.com/gravadigital/jiku-go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build ./bin/jiku
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)
	@echo "built bin/$(BINARY) ($(VERSION))"

.PHONY: install
install: ## Install jiku into $$GOPATH/bin
	go install -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

.PHONY: test
test: ## Run the tests (no network or bus needed)
	go test ./...

.PHONY: race
race: ## Run the tests under the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run the tests with a coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@echo "html: go tool cover -html=coverage.out"

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: ci
ci: ## fmt check + vet + race tests — the gate CI and the release both run
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run: make fmt" && exit 1)
	go vet ./...
	@# -race, not a plain `go test`: the package documents Client and the token sources as safe
	@# for concurrent use, and nats.go really does call the token handler and the async error
	@# handler from its own goroutines. Without the detector those promises are untested.
	go test -race ./...
	@echo "ok"

# `check` is kept as an alias because it reads better by hand; `ci` is the name the workflows
# use, matching nats-zitadel-auth-callout so the gate is called the same thing in both repos.
.PHONY: check
check: ci ## alias for `ci`

.PHONY: doc
doc: ## Serve the package documentation locally
	@echo "http://localhost:6060/pkg/$(PKG)/"
	go run golang.org/x/tools/cmd/godoc@latest -http=:6060

# Cross-compiled release binaries. Static by default: CGO is not used anywhere here.
#
# -trimpath strips the building machine's absolute paths out of the binary, so two people
# building the same tag get the same bytes and nobody ships their home directory in a stack trace.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: dist
dist: ## Cross-compile release binaries and checksums into ./dist
	@rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "  $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS) -s -w" -o $$out ./cmd/$(BINARY) || exit 1; \
	done
	@cd dist && sha256sum * > SHA256SUMS.txt && echo "  dist/SHA256SUMS.txt"

# Cutting a release is `git tag` and nothing else — the tag IS the release for a Go module. This
# target only refuses the mistakes that cannot be undone afterwards, because proxy.golang.org
# caches a published tag immutably: a wrong tag is not fixable, only superseded.
.PHONY: tag
tag: ## Check and create a release tag: make tag VERSION=v1.2.3
	@# VERSION defaults to `git describe` for the build targets, so testing it for emptiness
	@# would silently accept that default here and try to tag whatever it produced. `origin`
	@# asks where the value came from, which is the actual question.
	@test "$(origin VERSION)" = "command line" \
		|| (echo "usage: make tag VERSION=v1.2.3" && exit 1)
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' \
		|| (echo "error: $(VERSION) is not a vMAJOR.MINOR.PATCH tag" && exit 1)
	@# An `if`, not `cmd && (...) || true`: that form swallows the exit 1 of its own error
	@# branch along with rev-parse's failure, so the check printed and carried on.
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		echo "error: tag $(VERSION) already exists. A published version cannot be"; \
		echo "       reused: the module proxy has cached it. Pick the next one."; \
		exit 1; \
	fi
	@test -z "$$(git status --porcelain)" \
		|| (echo "error: the working tree is dirty; commit before tagging" && exit 1)
	@branch=$$(git rev-parse --abbrev-ref HEAD); \
		test "$$branch" = "main" \
		|| (echo "error: on branch $$branch, not main. Releases are cut from main." && exit 1)
	@$(MAKE) --no-print-directory ci
	@# patsubst, not $(VERSION:v=): the substitution reference matches a SUFFIX, so it would
	@# leave the leading v in place and look for a heading that never exists.
	@grep -q "^## \[$(patsubst v%,%,$(VERSION))\]" CHANGELOG.md \
		|| (echo "error: CHANGELOG.md has no '## [$(patsubst v%,%,$(VERSION))]' section" \
		    && echo "       add it before tagging, so the release notes are not written after" \
		    && exit 1)
	git tag -a "$(VERSION)" -m "$(VERSION)"
	@echo
	@echo "Created $(VERSION). Nothing is published yet. To publish:"
	@echo "    git push origin main $(VERSION)"
	@echo
	@echo "That push is irreversible: the module proxy caches the tag permanently."

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out
