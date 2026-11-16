FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bindle-ingest ./cmd/bindle-ingest

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bindle-ingest /bindle-ingest
COPY pricing.example.json /etc/bindle/pricing.json
EXPOSE 4318
USER nonroot
ENTRYPOINT ["/bindle-ingest"]
