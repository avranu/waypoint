# make           build web app + server into bin/waypoint, wp CLI into bin/cli
# make cli       cross-compile the wp CLI into bin/cli (served at /cli)
# make dev       run the server (:8080) and Vite (:5173) with live reload
# make test      go vet, go test (-timeout 30m as in CI), frontend typecheck (CI adds -race)
# make lint      go vet, gofmt check, golangci-lint (needs: make tools), wplint
# make sast      gosec (advisory; see README Tooling)
# make vuln      govulncheck, failing only for what this tree adds over origin/main (deploy/advisories.sh; needs network and jq)
# make check     lint + sast + vuln, then test: all run even if one fails (the full local gate)
# make prepush   CI's lint job on what changed (the pre-commit pre-push hook)
# make tools     install pinned Go QA binaries into GOPATH/bin
# make ci-image  build and push CI's job image, pin ci.yml to it (.forgejo/ci-image)
# make ops-lint  shellcheck/hadolint/actionlint/kubeconform on deploy + CI files
# make deploy    rebuild and restart the compose deployment (reads deploy/.env)
# make deploy-remote  ship HEAD to DEPLOY_HOST and deploy it there
# make rollback-remote  go back to the previous deploy on DEPLOY_HOST
# make deploy-status    what DEPLOY_HOST is running
# make restart-remote SERVICE=meaning  restart one helper service there
# make release-dry-run  the next version and release notes (deploy/release.sh)
# make help      list targets

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Renovate bumps these pins; when changing JS tool versions, update the pre-commit additional_dependencies pins too.
GOLANGCI_VERSION ?= v2.14.0
GOSEC_VERSION ?= v2.29.0
# gosec v2.29.0 bundles an x/tools too old to read Go 1.27 export data, so
# `make tools` builds it against this newer x/tools instead of `go install`.
GOSEC_XTOOLS_VERSION ?= v0.52.0
GOVULNCHECK_VERSION ?= v1.9.0
ACTIONLINT_VERSION ?= v1.7.12
KUBECONFORM_VERSION ?= v0.8.0

.PHONY: e2e all build web server cli cross-check test lint golint wplint wplint-code sast vuln check prepush prepush-go prepush-ops prepush-web tools ci-image ops-lint dev docker deploy deploy-remote rollback-remote deploy-status restart-remote need-deploy-host release-dry-run clean help

all: build

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-9s %s\n", $$1, $$2}'

build: web server cli ## web app + server binary into bin/waypoint, CLI into bin/cli

web: ## build the React app into web/dist
	cd web && npm ci --no-audit --no-fund && npm run build

server: ## build bin/waypoint (embeds whatever is in web/dist)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/waypoint ./cmd/waypoint

# The server serves these at /cli from the cli folder next to its binary.
cli: ## cross-compile wp (linux/darwin amd64+arm64, windows/amd64; WP_TARGETS="os/arch ..." narrows) + SHA256SUMS into bin/cli
	cmd/wp/build-all.sh bin/cli $(VERSION)

# Go packages are scoped explicitly (never ./...): web/node_modules can contain
# third-party Go code from npm deps, which must not enter vet/test/lint.
GOPKGS := ./cmd/... ./internal/... ./web

GOTOOLS = PATH="$$(go env GOPATH)/bin:$$PATH"
WEBDEPS = { [ -x node_modules/.bin/tsc ] || npm ci --no-audit --no-fund; }

# Heavy steps wait for the host's "tests" wp slot so concurrent agents' suites don't overload one machine.
SLOT ?= $(shell echo ",$$WAYPOINT_SLOTS_HELD" | grep -q ",tests=" || { wp slot -h >/dev/null 2>&1 && echo wp slot run tests --; })

# wp testsum records the run against this exact tree so wp ship can report it.
TESTSUM ?= $(if $(SLOT),$(shell wp testsum -h >/dev/null 2>&1 && echo wp testsum -- || echo '$(SLOT)'))

test: ## backend tests + frontend typecheck (portable: no -race, needs cgo)
	go vet $(GOPKGS)
	$(TESTSUM) go test -count=1 -timeout 30m -cover $(GOPKGS)
	cd web && $(WEBDEPS) && npm run typecheck

tools: ## install pinned Go QA binaries
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	d=$$(mktemp -d) && cd $$d && go mod init gosecbuild >/dev/null 2>&1 && \
	  go get github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) golang.org/x/tools@$(GOSEC_XTOOLS_VERSION) && \
	  go build -o "$$(go env GOPATH)/bin/gosec" github.com/securego/gosec/v2/cmd/gosec && rm -rf $$d
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	go install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

ci-image: ## build and push CI's job image if the registry lacks it, and pin ci.yml to it
	.forgejo/ci-image/build.sh

lint: cross-check golint ## vet + format check + full lint + wplint + wp cross-builds
	$(MAKE) wplint

golint: ## go vet, gofmt check and golangci-lint over GOPKGS
	go vet $(GOPKGS)
	test -z "$$(gofmt -l cmd internal)"
	$(GOTOOLS) $(SLOT) golangci-lint run $(GOPKGS)

# Waypoint's own rules from AGENTS.md and REVIEW.md (internal/wplint, WP-707).
# A finding is fixed or waived on its line: //wplint:ignore RULE why.
wplint: wplint-code ## Waypoint's own analyzers + the migration and e2e-wait checks against origin/main
	go run ./cmd/wplint repo -base origin/main

# Runs every wplint check with no base to diff against, since on main there is no origin/main to compare with.
wplint-code: ## Waypoint's own analyzers over cmd and internal
	$(SLOT) go run ./cmd/wplint ./cmd/... ./internal/...

# wp ships for Windows and macOS too (make cli); build those here so a
# Unix-only call in a shared file fails before the release job does.
cross-check: ## build wp for windows/amd64 and darwin/arm64 (no output)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/wp
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/wp

prepush: prepush-go prepush-ops prepush-web ## CI's lint job, as the pre-push hook runs it

prepush-go: cross-check
	go vet $(GOPKGS)
	test -z "$$(gofmt -l cmd internal)"
	$(GOTOOLS) golangci-lint run $(GOPKGS)
	go run ./cmd/wplint ./cmd/... ./internal/...
	go run ./cmd/wplint repo -base origin/main

prepush-ops:
	$(GOTOOLS) actionlint -config-file .forgejo/actionlint.yaml .forgejo/workflows/ci.yml .forgejo/workflows/advisories.yml
	$(GOTOOLS) actionlint .github/workflows/*.yml
	$(GOTOOLS) kubeconform -strict -summary deploy/k8s/waypoint.yaml
	@go run ./cmd/wp ci lint || true

prepush-web:
	cd web && $(WEBDEPS) && npm run typecheck && npm run format:check

# gosec's G104 is skipped because golangci-lint's errcheck already reports unchecked errors.
sast: ## gosec SAST (accepted findings are marked `#nosec <rule> -- why`)
	$(GOTOOLS) $(SLOT) gosec -exclude=G104 -exclude-dir=testdata $(GOPKGS)

vuln: ## govulncheck: fails only for called vulnerabilities this tree adds over origin/main (deploy/advisories.sh)
	$(GOTOOLS) deploy/advisories.sh check go pr

# Quick steps run first: lint takes minutes and tests most of an hour, so a lint finding shows before the long wait.
check: ## the full local gate (backend); web: cd web && npm run typecheck && npm run format:check && npm run audit && npm run build (lint dormant, see README)
	@$(MAKE) --no-print-directory -k lint sast vuln test || { echo 'make check: a step above failed; every step ran' >&2; exit 1; }

# Dev sets WAYPOINT_BASE_URL to Vite (:5173), so invite and notification links point there.
e2e: build ## browser checks (Playwright) against a throwaway server, all four looks; ARGS="05 07" picks suites, E2E_LOOKS=one does one look each
	cd e2e && $(SLOT) ./run.sh $(ARGS)

dev: ## API on :8080 + Vite on :5173 with live reload
	@echo "API on :8080, app on http://localhost:5173"
	trap 'kill 0' EXIT INT TERM; \
	WAYPOINT_BASE_URL=http://localhost:5173 WAYPOINT_LOG_FORMAT=text go run ./cmd/waypoint & \
	cd web && npm run dev

# hadolint is not a Go tool, so it runs from its pinned image.
HADOLINT_IMAGE ?= hadolint/hadolint:v2.14.0

ops-lint: ## lint CI workflow, k8s manifest, shell scripts (+ Dockerfile via docker)
	actionlint -config-file .forgejo/actionlint.yaml .forgejo/workflows/ci.yml .forgejo/workflows/advisories.yml
	actionlint .github/workflows/*.yml
	kubeconform -strict -summary deploy/k8s/waypoint.yaml
	@if git ls-files '*.sh' | grep -q .; then shellcheck $$(git ls-files '*.sh'); fi
	@if command -v docker >/dev/null; then for f in deploy/Dockerfile .forgejo/ci-image/Dockerfile; do docker run --rm -i $(HADOLINT_IMAGE) < $$f || exit 1; done; else echo "skip hadolint (no docker)"; fi
	@if git ls-files '*.sh' | grep -q .; then shellcheck $$(git ls-files '*.sh'); fi

docker: ## build the image as waypoint:$(VERSION) and waypoint:latest
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t waypoint:$(VERSION) -t waypoint:latest .

deploy: ## rebuild and (re)start the compose deployment
	WAYPOINT_VERSION=$(VERSION) docker compose -p waypoint -f deploy/compose.yaml up -d --build

# SITE_NAMES comes from the environment locally, else the per-user site-names file; CI uses its secret
-include deploy/site.mk
export SITE_NAMES
DEPLOY_HOST ?=
DEPLOY_VERSION ?= $(shell git describe --tags --always 2>/dev/null)
need-deploy-host:
	@test -n "$(DEPLOY_HOST)" || { echo 'set DEPLOY_HOST, the production host'"'"'s ssh name (environment or deploy/site.mk)' >&2; exit 2; }

deploy-remote: need-deploy-host ## ship HEAD to $(DEPLOY_HOST) and deploy it there (health-checked, rolls back on failure)
	git archive --format=tar HEAD | ssh $(DEPLOY_HOST) 'set -e; rm -rf waypoint.new; mkdir waypoint.new; tar -x -C waypoint.new; \
	  bash waypoint.new/deploy/remote-deploy.sh deploy $(DEPLOY_VERSION) --staged'

rollback-remote: need-deploy-host ## swap $(DEPLOY_HOST) back to its previous deploy
	ssh $(DEPLOY_HOST) 'bash waypoint/deploy/remote-deploy.sh rollback'

deploy-status: need-deploy-host ## the version and containers running on $(DEPLOY_HOST), and what its deployer will do next
	ssh $(DEPLOY_HOST) 'bash waypoint/deploy/remote-deploy.sh status; if [ -x bin/waypoint-deployer ]; then bin/waypoint-deployer status; else echo "no deployer installed (deploy/setup-cd.sh)"; fi'

restart-remote: need-deploy-host ## restart one helper service on $(DEPLOY_HOST): make restart-remote SERVICE=meaning (or opencode)
	$(if $(filter-out 1,$(words $(SERVICE)))$(filter-out meaning opencode,$(SERVICE)),$(error say SERVICE=meaning or SERVICE=opencode))
	ssh $(DEPLOY_HOST) 'bash waypoint/deploy/remote-deploy.sh restart $(filter meaning opencode,$(SERVICE))'

release-dry-run: ## the next version and release notes CI would publish
	deploy/release.sh --dry-run

clean: ## remove build output (keeps data/; bin/ includes bin/cli)
	rm -rf bin waypoint web/.vite
	find web/dist -mindepth 1 ! -name .gitkeep -delete
