FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/pin-glide-db-tools ./cmd/pin-glide-db-tools
COPY internal ./internal
COPY db ./db
RUN CGO_ENABLED=0 go build -o /out/pin-glide-db-tools ./cmd/pin-glide-db-tools \
 && CGO_ENABLED=0 go build -o /out/psqldef github.com/sqldef/sqldef/v3/cmd/psqldef \
 && mkdir -p db/migrations/release db/migrations/pin_glide db/scripts

FROM postgres:18-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/pin-glide-db-tools /pin-glide-db-tools
COPY --from=build /out/psqldef /usr/local/bin/psqldef
COPY --from=build /src/db /app/db
USER 65532:65532
ENTRYPOINT ["/pin-glide-db-tools"]
CMD ["migrate"]
