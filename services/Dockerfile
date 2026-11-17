FROM golang:1.27 AS build
WORKDIR /src
COPY . .
# Standard library only: no module downloads needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/bindle-ingest ./cmd/bindle-api ./cmd/bindle-migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /out/bindle-api /out/bindle-migrate /
COPY pricing.example.json /etc/bindle/pricing.json
# Inside the container, listen on all interfaces; exposure is governed by the
# port publish (compose: 127.0.0.1 only) or the ALB target group.
ENV BINDLE_LISTEN=:4318
EXPOSE 4318
USER nonroot
# Default entrypoint is ingest; use --entrypoint /bindle-api or /bindle-migrate for the others.
ENTRYPOINT ["/bindle-ingest"]
