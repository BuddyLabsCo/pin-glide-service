.PHONY: build test format format-check lint

build:
	go build ./...

test:
	./scripts/quality/test.sh

format:
	./scripts/quality/format.sh

format-check:
	./scripts/quality/format.sh --check

lint:
	./scripts/quality/golangci-lint.sh
	./scripts/quality/check-file-size.sh
