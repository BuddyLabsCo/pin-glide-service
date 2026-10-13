.PHONY: build test format format-check lint lint-sql format-sql

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

lint-sql:
	./scripts/quality/lint-sql.sh

format-sql:
	uvx --from sqlfluff==4.3.0 sqlfluff fix db/schema/release db/schema/pin_glide
