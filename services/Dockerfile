FROM golang:1.27 AS build
WORKDIR /src
COPY . .
# Dependencies are vendored (services/vendor), so no module proxy is needed.
RUN mkdir -p /out/data/payloads && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/bindle-ingest ./cmd/bindle-api ./cmd/bindle-migrate ./cmd/bindle-processor ./cmd/bindle-alerts

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /out/bindle-api /out/bindle-migrate /out/bindle-processor /out/bindle-alerts /
# Local payload store (file:///data/payloads); owned by the runtime user so a
# fresh Docker volume mounted here is writable. Unused when payloads go to S3.
COPY --from=build --chown=65532:65532 /out/data /data
COPY pricing.example.json /etc/bindle/pricing.json
# Public CA bundle for verifying Amazon RDS server certificates (PGSSLROOTCERT).
COPY certs/rds-global-bundle.pem /etc/bindle/rds-ca.pem
# Inside the container, listen on all interfaces; exposure is governed by the
# port publish (compose: 127.0.0.1 only) or the ALB target group.
ENV BINDLE_LISTEN=:4318
EXPOSE 4318
USER nonroot
# Default entrypoint is ingest; use --entrypoint /bindle-api or /bindle-migrate for the others.
ENTRYPOINT ["/bindle-ingest"]
