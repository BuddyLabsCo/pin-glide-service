FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/pin-glide-release ./cmd/pin-glide-release
COPY internal ./internal
COPY db ./db
RUN CGO_ENABLED=0 go build -o /out/pin-glide-release ./cmd/pin-glide-release \
 && CGO_ENABLED=0 go build -o /out/psqldef github.com/sqldef/sqldef/v3/cmd/psqldef \
 && mkdir -p db/migrations/release db/migrations/pin_glide db/scripts

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/pin-glide-release /pin-glide-release
COPY --from=build /out/psqldef /usr/local/bin/psqldef
COPY --from=build /src/db /app/db
USER 65532:65532
ENTRYPOINT ["/pin-glide-release"]
CMD ["migrate"]
