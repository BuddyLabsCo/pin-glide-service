FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/pin-glide-api ./cmd/pin-glide-api

FROM alpine:3.22
COPY --from=build /out/pin-glide-api /pin-glide-api
USER 65532:65532
ENTRYPOINT ["/pin-glide-api"]
