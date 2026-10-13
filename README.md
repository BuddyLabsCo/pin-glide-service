# Pin Glide Service

The service side of Pin Glide: one Go process that authenticates Android
Senders, authorizes Pairings, supplies model credentials, relays live ciphertext
to Windows Receivers and owns the shared HTTP/WSS contract.

## Local build and tests

Prerequisites: the Go toolchain from `go.mod`, Node.js 24 for the shell-helper
tests, and Docker only for the tagged PostgreSQL integration tests.

```sh
make build          # compile every Go package
make test           # node helper tests, then go test ./...
make format-check   # gofmt check over handwritten Go sources
make lint           # pinned golangci-lint, then the source-size check
```

The default tests need no credentials and no database. They start the composed
service on a real loopback listener and assert the sanitized `/health` contract,
routing fallbacks, the shared problem-details error model and bounded shutdown.

PostgreSQL-backed integration tests are explicit and use a disposable,
loopback-only database:

```sh
go test -tags=integration ./test/integration/...
```

## Running the API process

`cmd/pin-glide-api` requires a reachable PostgreSQL database and reads its
configuration from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `PIN_GLIDE_ENV` | `development` | `development` or `production` |
| `PIN_GLIDE_API_ADDRESS` | `:8080` | Listen address of the API process |
| `PIN_GLIDE_DATABASE_URL` | local development DSN | PostgreSQL connection string; required in production |

## Not available yet

The repository is implemented ticket by ticket. SQL quality checks, schema
generation, the release CLI, Compose installation and local development, CI
workflows and the published HTTP/WSS contract assets arrive with their own
tickets; `make build` and `make test` are the documented entrypoints today.
