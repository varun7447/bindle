FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bindle-ingest ./cmd/bindle-ingest

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /bindle-ingest
COPY pricing.example.json /etc/bindle/pricing.json
# Inside the container, listen on all interfaces; exposure is governed by the
# port publish (compose: 127.0.0.1 only) or the ALB target group.
ENV BINDLE_LISTEN=:4318
EXPOSE 4318
USER nonroot
ENTRYPOINT ["/bindle-ingest"]
