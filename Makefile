.PHONY: build test generate check-contracts format format-check lint lint-sql format-sql dev

build:
	go build ./...

test:
	./scripts/quality/test.sh

generate:
	./scripts/codegen/generate.sh

check-contracts:
	./scripts/quality/check-contracts.sh

format:
	./scripts/quality/format.sh

format-check:
	./scripts/quality/format.sh --check

lint:
	./scripts/quality/golangci-lint.sh
	./scripts/quality/check-file-size.sh

lint-sql:
	./scripts/quality/lint-sql.sh

format-sql:
	./scripts/quality/sqlfluff.sh fix db/schema/release db/schema/pin_glide

dev:
	./scripts/dev/dev.sh
