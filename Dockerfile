# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first so code edits don't re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go SQLite means CGO stays off and the binary is fully static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /backoffice ./cmd/backoffice

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /backoffice /usr/local/bin/backoffice

ENV DATA_DIR=/data PORT=8080
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/backoffice"]
