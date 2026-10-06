FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orchard ./cmd/orchard

FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl \
  && rm -rf /var/lib/apt/lists/*

RUN useradd --system --uid 10001 --user-group --home-dir /data --shell /usr/sbin/nologin orchard \
  && mkdir -p /data \
  && chown orchard:orchard /data

COPY --from=build /out/orchard /usr/local/bin/orchard

ENV ORCHARD_DATA_DIR=/data \
  ORCHARD_ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080
USER orchard

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["orchard"]
