# mailvault

Back up mail from multiple sources to multiple targets.

> **Status:** early scaffolding. The application does not do anything yet.

## planned scope

- Read mail from several sources (for example IMAP accounts).
- Write the same messages to several targets (for example local Maildir, S3-compatible object storage).
- Run once per invocation, report per-source and per-target results, and exit non-zero on failure.

## development

Requires Go (see `go.mod`) and [golangci-lint](https://golangci-lint.run/).

```sh
make build   # build dist/mailvault
make lint    # gofmt check and golangci-lint
make test    # unit tests
```

## license

[GNU Affero General Public License v3.0](LICENSE).
