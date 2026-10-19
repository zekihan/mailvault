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

## delivery state upgrades

Successful deliveries are tracked separately for each configured target name.
Existing databases are upgraded automatically with a new `deliveries` table;
old message records are preserved, but cannot prove which targets received mail.
The first sync after this upgrade therefore conservatively delivers existing
messages again. Maildir and mbox targets may receive duplicate copies on that
first run. Subsequent syncs use per-target delivery state to skip them.
Keep target names stable; a new target name is treated as a new destination.

## authentication and storage behavior

- POP3 supports implicit TLS (`use_tls: true`, normally port 995), not STARTTLS.
  `start_tls: true` is rejected for POP3 before connecting; it never falls back
  to plaintext authentication. Explicit plaintext POP3 remains available with
  both flags false for trusted local connections.
- POP3 does not provide reliable server arrival dates. `--since` and `--until`
  are rejected when any active source is POP3 rather than silently ignored.
- OAuth2-only source configurations are rejected until OAuth2 authentication
  is implemented. Sources currently require `password_ref`.
- A sync only constructs and initializes sources/targets referenced by pairs.
  `--dry-run` fetches source messages, but does not initialize/write targets or
  update persistent state. Existing state is opened read-only; absent state is
  represented by a private in-memory database.
- Filesystem targets honor `create_if_missing: false`: the mbox file, or all
  Maildir directories, must already exist. Maildir publication never replaces
  an existing message. Cooperating mbox writers lock the mailbox while appending
  and rolling back failed appends; rollback failure disables further writes
  through that target instance. External mbox writers must use compatible locks.

### S3 credentials

Omit `credentials_ref` to use the standard AWS credential chain (environment,
shared profiles, workload roles, and so on). To explicitly select credentials,
set `credentials_ref` to an `env:` or `file:` reference resolving to a JSON object
with `access_key_id` and `secret_access_key`, plus optional `session_token`.
Explicit empty or malformed values fail without falling back to ambient AWS
credentials. Never commit the JSON credential object to configuration files.

## license

[GNU Affero General Public License v3.0](LICENSE).
