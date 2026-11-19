# Contributing to Bindle

Thanks for helping. Bug reports, fixes and new framework support are all welcome.

## Getting set up

You need Docker, Go 1.25+, Node 20+ and Python 3.9+.

```bash
make up        # local stack: OpenSearch, PostgreSQL, ingest, API, alerts
make console   # console at http://localhost:3000
make demo      # send a few demo runs
make test      # every test suite; CI runs the same on each pull request
```

The repository layout is in the [README](README.md); each folder has its own README.

## Pull requests

- Keep changes small and focused, with a clear commit message.
- Add or update tests with every change; `make test` must pass.
- Go code is `gofmt`-formatted and uses the standard library unless there is a stated reason for a dependency.
- Follow the [design rules](docs/architecture.md#design-rules). In particular: the tenant comes only from the authenticated key or session, the SDK must never break or slow the customer's agent, redaction stays on by default, and model prices are never guessed.

## License of contributions

By contributing, you agree that your contributions are licensed under the [Apache License, Version 2.0](LICENSE), the same license as the project (section 5 of the license).
