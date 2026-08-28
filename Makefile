GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

.PHONY: lint
lint:
	go run $(GOLANGCI_LINT) run
