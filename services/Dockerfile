FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/bindle-ingest ./cmd/bindle-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /out/bindle-api /
COPY pricing.example.json /etc/bindle/pricing.json
# Inside the container, listen on all interfaces; exposure is governed by the
# port publish (compose: 127.0.0.1 only) or the ALB target group.
ENV BINDLE_LISTEN=:4318
EXPOSE 4318
USER nonroot
# Default entrypoint is ingest; run the query API with --entrypoint /bindle-api.
ENTRYPOINT ["/bindle-ingest"]
