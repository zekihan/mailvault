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

### content-identity upgrade

Delivery identities now use the SHA-256 of the actual raw message bytes, not
the sender-controlled Message-ID. Different content sharing an ID is preserved;
identical raw content is deduplicated per target even if envelope metadata differs.
Legacy `mid:` receipts cannot prove which content was delivered and are ignored.
The first sync after this upgrade can therefore redeliver previously backed-up
mail once; Maildir and mbox may gain duplicate copies. New content-hash receipts
make subsequent runs stable. Existing `hash:` receipts remain usable.

### overlapping writers

Writer invocations sharing a canonical state database path serialize through
`<database>.sync.lock`, held through state updates and resource finalization.
Symlink aliases resolve to the same lock. Process termination releases ownership;
the empty sidecar intentionally remains. Do not delete or replace it while any
writer is running. Waiting Go callers can cancel through their context.
Dry runs do not acquire or create writer locks. Separate state databases do not
coordinate deliveries, even if they point at the same backup destination.
Do not hard-link SQLite state databases under different filenames.

## authentication and storage behavior

- Omitted source settings default to implicit TLS, a 30-second connection
  timeout, and a 60-second read timeout. Explicit `use_tls: false` and zero
  timeouts are preserved. IMAP `start_tls: true` selects STARTTLS instead of
  implicit TLS when `use_tls` is omitted.
  Indexed `MAILVAULT_SOURCES_0_*` environment overrides are not supported;
  define source settings in the configuration file and use `env:` credential
  references for secrets.
- POP3 supports implicit TLS (`use_tls: true`, normally port 995), not STARTTLS.
  `start_tls: true` is rejected for POP3 before connecting; it never falls back
  to plaintext authentication. Explicit plaintext POP3 remains available with
  both flags false for trusted local connections.
- POP3 does not provide reliable server arrival dates. `--since` and `--until`
  are rejected when any active source is POP3 rather than silently ignored.
- IMAP timestamp bounds apply to server `INTERNALDATE`, not the sender's Date
  header: `--since` is inclusive and `--until` is exclusive. Calendar searches
  are widened for timezone offsets, then exact bounds are applied locally.
  Fractional RFC3339 bounds are rounded up to preserve comparisons against
  second-resolution arrival dates. Bounds resolving to Unix epoch zero are
  rejected because the source interface reserves zero for an absent bound.
  Missing `INTERNALDATE` fails filtering rather than silently including mail.
- OAuth2-only source configurations are rejected until OAuth2 authentication
  is implemented. Sources currently require `password_ref`.
- Normal sync resolves credentials only for sources and targets referenced by
  pairs. Unused secret references remain untouched; explicit `validate-config`
  resolves all declared resources for connectivity checks. Source password files
  may end with one LF or CRLF, which is removed without stripping password spaces.
  Other embedded line breaks are rejected, including in env/literal passwords.
  S3 JSON credential files remain byte-preserving and can contain multiple lines.
- A sync only constructs and initializes sources/targets referenced by pairs.
  `--dry-run` fetches source messages, but does not initialize/write targets or
  update persistent state. Existing state is opened read-only; absent state is
  represented by a private in-memory database.
  State database paths are literal filesystem paths; characters such as `?`,
  `#`, and `%` cannot redirect the database or inject SQLite connection options.
- Filesystem targets honor `create_if_missing: false`: the mbox file, or all
  Maildir directories, must already exist. Maildir publication never replaces
  an existing message. Cooperating mbox writers lock the mailbox while appending
  and rolling back failed appends; rollback failure disables further writes
  through that target instance. External mbox writers must use compatible locks.
- Maildir syncs newly created directories and their immediate parents during
  initialization; unrelated existing ancestors need only traversal permissions.
  It syncs the publication directory after each link before acknowledging delivery.
  Failed publication sync attempts are rolled back and reported as failures. Directory
  sync must be supported by the platform/filesystem; otherwise Maildir fails
  closed. Tests cover sync ordering and injected failures, not physical power loss.

### S3 credentials

Omit `credentials_ref` to use the standard AWS credential chain (environment,
shared profiles, workload roles, and so on). To explicitly select credentials,
set `credentials_ref` to an `env:` or `file:` reference resolving to a JSON object
with `access_key_id` and `secret_access_key`, plus optional `session_token`.
Explicit empty or malformed values fail without falling back to ambient AWS
credentials. Never commit the JSON credential object to configuration files.

S3 string options reject malformed types instead of silently selecting defaults.
Ordinary object keys retain their existing format; oversized folder/UID components
use a bounded identity digest. Prefixes, including their trailing slash, must leave
room for the bounded object name, keeping every key within S3's 1,024-byte limit.

## failure reporting

Delivery and source/target finalization failures increment `TotalErrors`, and
the CLI exits non-zero after printing the report. JSON `Results[].Errors` contains
error strings, while `FinalizationErrors` contains resource-specific close errors.
The engine does not print close warnings to stdout, keeping `--json` parseable.
For Go callers, `Run` returns setup failures directly; delivery and finalization
failures are recorded in the returned statistics.
Resources are closed on setup failures too, including partially initialized
targets; cleanup errors are preserved alongside the original failure.

`validate-config --json` reports configuration validity, whether connectivity was
tested, source/target status, and error strings as one JSON document, including
failed validations. Connectivity testing still initializes targets and can create
missing destination directories/files; it does not require a state database.

## license

[GNU Affero General Public License v3.0](LICENSE).
