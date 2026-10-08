# AGENTS.md

## Commands

- `make build`: build `dist/mailvault`.
- `make lint`: check formatting and run golangci-lint.
- `make test` / `make test/race`: run unit tests.

## Conventions

- Entry point is `cmd/mailvault`; application code goes under `internal/`.
- Conventional Commits. Do not add author/co-author trailers.
- Never commit mail content, credentials or real account configuration.
