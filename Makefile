PROJECT_NAME := notifications
BINARY_NAME  := $(PROJECT_NAME)
SRC_DIR      := ./cmd
DOC_DIR      := ./internal/generated_docs
BUILD_DIR    := ./tmp/build
GO_MOD       := go.mod

SWAGGER_JSON := $(DOC_DIR)/swagger.json
EMBED_FILE   := $(DOC_DIR)/embedded.go

# Docker image details
DOCKER_REPO      ?= ghcr.io/pfisterer/$(PROJECT_NAME)
DOCKER_TAG       ?= $(shell cat VERSION 2>/dev/null || echo "dev")

.DEFAULT_GOAL := all

.PHONY: publish all build clean doc check-swag generate-docs run dev test vet tidy help docker-build helm-update bump version-check

all: generate-docs build

# ── Documentation ─────────────────────────────────────────────────────────────

check-swag:
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@latest

# Generate swagger.json from the annotations and write embedded.go
generate-docs: check-swag
	@echo "📚 Generating swagger.json..."
	@mkdir -p $(DOC_DIR)
	@set -e; swag init -g doc.go --dir $(SRC_DIR),./internal/web,./internal/model -o $(DOC_DIR) --outputTypes json
	@cp VERSION $(DOC_DIR)/VERSION
	@printf '%s\n' \
		'package generated_docs' \
		'' \
		'import _ "embed"' \
		'' \
		'//go:embed swagger.json' \
		'var SwaggerJSON string' \
		'//go:embed VERSION' \
		'var Version string' \
		> $(EMBED_FILE)
	@echo "✅ Docs generated in $(DOC_DIR)/"

doc: generate-docs

# ── Build ─────────────────────────────────────────────────────────────────────

build: check-modules
	@echo "🔨 Building Go binary..."
	@mkdir -p $(BUILD_DIR)
	@set -e; GOOS=$${BUILD_GOOS:-$$(go env GOOS)} GOARCH=$${BUILD_GOARCH:-$$(go env GOARCH)} go build -o $(BUILD_DIR)/$(BINARY_NAME) $(SRC_DIR)
	@echo "✅ Binary: ./$(BUILD_DIR)/$(BINARY_NAME)"

check-modules:
	@test -f $(GO_MOD) || (echo "❌ $(GO_MOD) missing; run 'go mod init' first."; exit 1)

# ── Dev / Run ─────────────────────────────────────────────────────────────────

dev:
	API_MODE=development air

run: build
	@./$(BUILD_DIR)/$(BINARY_NAME)

# ── Quality ───────────────────────────────────────────────────────────────────

vet: generate-docs
	@go vet ./...

test: generate-docs
	@echo "🧪 Running tests..."
	@go test ./...
	@echo "✅ Tests passed"

tidy:
	go mod tidy

# ── Docker ────────────────────────────────────────────────────────────────────

docker-build: generate-docs
	docker build --progress=plain -t "$(DOCKER_REPO):$(DOCKER_TAG)" .

# ── Versioning ────────────────────────────────────────────────────────────────
# VERSION is the single source of truth; Chart.yaml has to agree with it. `version-check` runs in CI and FAILS on a mismatch rather than quietly rewriting the file — rewriting would hide exactly the mistake it is meant to catch.

# make bump V=1.2.3
bump:
	@test -n "$(V)" || { echo "usage: make bump V=<x.y.z>"; exit 1; }
	@printf '%s' "$(V)" > VERSION
	@$(MAKE) --no-print-directory helm-update

version-check:
	@v=$$(cat VERSION | tr -d '\n'); \
	cv=$$(awk '/^version:/{print $$2}' helm-chart/Chart.yaml); \
	av=$$(awk '/^appVersion:/{gsub(/"/,"",$$2); print $$2}' helm-chart/Chart.yaml); \
	if [ "$$v" != "$$cv" ] || [ "$$v" != "$$av" ]; then \
		echo "✗ VERSION is $$v, Chart.yaml says version=$$cv appVersion=$$av"; \
		echo "  fix with: make bump V=$$v"; \
		exit 1; \
	fi; \
	echo "✅ version $$v is consistent"

# The defaults alone do not pass the schema on purpose (no tokens, no secret, no mail server), so the lint names an existing Secret the way every deployment does and only logs mails.
helm-update:
	helm lint helm-chart/ --set notifications.existingSecret=lint --set notifications.mail.mode=log
	@echo "✅ Helm chart linted successfully."
	@VERSION=$$(cat VERSION | tr -d '\n'); \
	sed -e "s/^version: .*/version: $$VERSION/" \
	    -e "s/^appVersion: .*/appVersion: \"$$VERSION\"/" \
	    helm-chart/Chart.yaml > helm-chart/Chart.yaml.tmp; \
	mv helm-chart/Chart.yaml.tmp helm-chart/Chart.yaml; \
	echo "✅ Updated helm-chart/Chart.yaml to version $$VERSION"

clean:
	@rm -rf ./tmp/build

help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "  all              → Generate docs and build binary (default)"
	@echo "  generate-docs    → Run swag to regenerate swagger.json + embedded.go"
	@echo "  build            → Compile Go binary"
	@echo "  run              → Build and run"
	@echo "  dev              → Live-reload dev server (requires air)"
	@echo "  test / vet       → Go tests / go vet"
	@echo "  docker-build     → Build Docker image"
	@echo "  bump V=x.y.z     → Set VERSION and the chart version"
	@echo "  publish          → Release from this machine (image + chart to GHCR, push)"

# make publish — release from this machine instead of waiting for CI, for test and stable versions alike: runs the checks, builds the image for the cluster (linux/amd64; the build stage runs natively and cross-compiles, see the Dockerfile), pushes it and the Helm chart to GHCR, and only then pushes the commits. CI then finds the version published and just verifies that it builds; tags and GitHub releases of stable versions stay with CI. Refuses a dirty tree and a version that is already published, so what is in the registry is always exactly one commit. Needs `docker login ghcr.io` and `helm registry login ghcr.io` with a token that may write packages.
publish: version-check test
	@test -z "$$(git status --porcelain)" || { echo "✗ uncommitted changes — commit first"; exit 1; }
	@set -e; V=$$(tr -d '\n' < VERSION); I=ghcr.io/pfisterer/notifications; \
	if docker buildx imagetools inspect "$$I:$$V" >/dev/null 2>&1; then echo "✗ $$I:$$V is already published — bump the version first"; exit 1; fi; \
	echo "🏗️  $$I:$$V (linux/amd64)"; \
	docker buildx build --platform linux/amd64 --tag "$$I:$$V" --tag "$$I:latest" --push .; \
	mkdir -p tmp/chart && find tmp/chart -name '*.tgz' -delete && helm package helm-chart/ --destination tmp/chart >/dev/null; \
	helm push tmp/chart/notifications-$$V.tgz oci://ghcr.io/pfisterer/charts; \
	git push; \
	echo "✅ $$I:$$V and its chart are published, commits pushed — CI only verifies"
