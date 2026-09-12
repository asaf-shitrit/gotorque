GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
GO_CRAP := github.com/padiazg/go-crap@v0.5.1
COVERAGE_PROFILE := coverage.out
# Cross-package coverage: a function exercised by another package's tests is
# tested, and a per-package profile reports it as 0%. Point this at a package
# subset if a full-profile run ever gets slow.
COVERPKG ?= ./...

# CRAP = CC² × (1 − coverage)³ + CC. Above this a function is expensive to test
# and risky to change. Loosen only with a reason — the point is to catch a
# complex function that no test reaches, which gocyclo/gocognit cannot see.
CRAP_THRESHOLD ?= 30

COVER_FLAGS := -coverprofile=$(COVERAGE_PROFILE) -coverpkg=$(COVERPKG)
CRAP_SCAN := go run $(GO_CRAP) scan . --coverage-profile $(COVERAGE_PROFILE) \
	--threshold $(CRAP_THRESHOLD) --no-progress

.PHONY: lint
lint:
	go run $(GOLANGCI_LINT) run

# Single coverage step: it runs the unit tests and leaves the profile the CRAP
# targets read, so the hook never runs the suite twice.
.PHONY: cover
cover:
	go test $(COVER_FLAGS) ./...

# Report-only: worst offenders, always exits 0.
.PHONY: crap
crap: cover
	$(CRAP_SCAN) --top 25

# Gate: runs the suite, then exits 1 when any function is above the threshold.
.PHONY: crap-check
crap-check: cover
	$(CRAP_SCAN) --fail-above

# Same gate against an existing $(COVERAGE_PROFILE); no test run. This is what
# .githooks/pre-commit calls after `make cover`.
.PHONY: crap-scan
crap-scan:
	$(CRAP_SCAN) --fail-above

# One-time per clone: point git at the versioned hooks. Undo with
# `git config --unset core.hooksPath`.
.PHONY: hooks
hooks:
	git config core.hooksPath .githooks
	@echo "hooks installed: pre-commit runs make lint, make cover, make crap-scan"
	@echo "bypass a single commit with: git commit --no-verify"
