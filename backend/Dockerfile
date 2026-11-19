FROM golang:1.27 AS build
# Where modules are downloaded from. The default is Go's public proxy; set
# GOPROXY=direct to fetch from the source repositories instead (for networks
# that can't reach proxy.golang.org). go.sum pins every module's checksum
# either way.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
# Dependencies first, so they are cached across source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out/data/payloads && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/bindle-ingest ./cmd/bindle-api ./cmd/bindle-migrate ./cmd/bindle-processor ./cmd/bindle-alerts

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /out/bindle-api /out/bindle-migrate /out/bindle-processor /out/bindle-alerts /
# Local payload store (file:///data/payloads); owned by the runtime user so a
# fresh Docker volume mounted here is writable. Unused when payloads go to S3.
COPY --from=build --chown=65532:65532 /out/data /data
COPY pricing.json /etc/bindle/pricing.json
# Public CA bundle for verifying Amazon RDS server certificates (PGSSLROOTCERT).
COPY certs/rds-global-bundle.pem /etc/bindle/rds-ca.pem
# Inside the container, listen on all interfaces; exposure is governed by the
# port publish (compose: 127.0.0.1 only) or the ALB target group.
ENV BINDLE_LISTEN=:4318
EXPOSE 4318
USER nonroot
# Default entrypoint is ingest; use --entrypoint /bindle-api or /bindle-migrate for the others.
ENTRYPOINT ["/bindle-ingest"]
