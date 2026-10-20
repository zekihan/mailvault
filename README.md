# mailvault

Back up mail from multiple sources to multiple targets.

Read mail from IMAP or POP3 accounts and back it up to Maildir, mbox, or
S3-compatible storage. Each invocation runs once, reports per-source and
per-target results, and exits non-zero on failure.

Supported hosts are macOS, Linux, Windows, and the BSDs covered by the platform
locking implementations. Other platforms fail closed when safe locking is
unavailable. Press Ctrl+C to cancel an active sync and close its resources;
successful delivery receipts remain available to the next run.

## development

Requires Go (see `go.mod`) and [golangci-lint](https://golangci-lint.run/).

```sh
make build   # build dist/mailvault
make lint    # gofmt check and golangci-lint
make test    # unit tests
```

## incremental backups

IMAP syncs search for message UIDs but download bodies only for UIDs without a
successful delivery receipt for the current target. Receipts include the source,
account/server identity, folder, and UIDVALIDITY, so new targets, changed accounts,
and reset mailboxes are scanned safely. Failed deliveries remain eligible for retry.
Newly labeled or moved messages may have new folder UIDs; content deduplication
prevents writing identical bytes again by default. Maildir `preserve_folders: true`
scopes delivery identities by folder so duplicate content is retained in each
preserved folder.

```sh
go run ./cmd/mailvault --config ./config.yaml sync
# Download all selected messages again; existing content is still deduplicated.
go run ./cmd/mailvault --config ./config.yaml sync --force-fetch-all
```

Older state has no trustworthy UIDVALIDITY receipts and needs one full scan after
this upgrade. Dry runs use existing receipts but never save new ones. Date filters
still apply, including with `--force-fetch-all`. Folder progress totals count the
pending server-search candidates, before exact date filtering and deduplication.
POP3 uses stable UIDL identifiers to skip successful deliveries. Servers that
explicitly reject UIDL fall back to full downloads and content deduplication.
Malformed UIDL responses fail the sync rather than guessing message identities.
POP3 date filtering uses the sender's Date header after retrieval, so excluded
messages may need to be retrieved again on later filtered runs.

IMAP folder UIDVALIDITY, UIDNEXT, and available MODSEQ values are recorded as
server observations. Download decisions use per-target receipts, not a global
high-water mark that could skip failed deliveries or holes left by date filters.
`max_connections` bounds the IMAP connection pool; downloads stream through
bounded batches and channels. Compact sets of delivered UIDs are excluded by the
server search, preserving holes from failures and date filters. If the exclusion
would exceed 4 KiB, the client filters the search result locally instead.

Downloads retry transient connection failures up to two times with a short
backoff. Stable IMAP UIDs and POP3 UIDLs already streamed in the current run are
excluded on reconnect. POP3 without UIDL does not retry after streaming a message,
because sequence numbers cannot safely identify it after reconnecting. Authentication
and protocol errors fail immediately; Ctrl+C cancels retries.

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

### pruning old metadata

`cleanup-state` previews fetched-message metadata older than a requested age.
Apply pruning explicitly with `--apply`; delivery and incremental UID receipts
are retained in either mode.

```sh
mailvault cleanup-state --older-than 720h
mailvault cleanup-state --older-than 720h --apply
```

## authentication and storage behavior

- Omitted source settings default to implicit TLS, a 30-second connection
  timeout, and a 60-second read timeout. Explicit `use_tls: false` and zero
  timeouts are preserved. `start_tls: true` selects STARTTLS instead of
  implicit TLS when `use_tls` is omitted, for both IMAP and POP3.
  Indexed environment overrides apply to existing configuration entries, for
  example `MAILVAULT_SOURCES_0_HOST`, `MAILVAULT_SOURCES_0_USE_TLS=false`, and
  `MAILVAULT_SOURCES_0_READ_TIMEOUT=60s`. List overrides use comma-separated values.
  Invalid boolean, integer, and duration overrides fail configuration loading.
- POP3 supports implicit TLS (normally port 995) and STARTTLS/STLS (normally
  port 110). STARTTLS failures never fall back to plaintext authentication.
  Both TLS flags may be false for explicitly trusted local connections; enabling
  both together is rejected.
- POP3 date bounds use the sender-provided Date header, which may be inaccurate
  and is not the server arrival date. Missing or invalid dates fail filtered
  syncs. `--since` is inclusive and `--until` is exclusive.
- IMAP timestamp bounds apply to server `INTERNALDATE`, not the sender's Date
  header: `--since` is inclusive and `--until` is exclusive. Calendar searches
  are widened for timezone offsets, then exact bounds are applied locally.
  Fractional RFC3339 bounds are rounded up to preserve comparisons against
  second-resolution arrival dates. Bounds resolving to Unix epoch zero are
  rejected because the source interface reserves zero for an absent bound.
  Missing `INTERNALDATE` fails filtering rather than silently including mail.
- Sources accept either `password_ref` or `oauth2`, never both. Microsoft OAuth uses device login;
  Google uses desktop loopback login with PKCE. Both refresh automatically and
  store tokens in the OS keyring by default, with an explicit private token-file
  option for headless hosts. See [OAuth setup](docs/oauth.md). No client secret
  is needed for these public-client flows.
- Credential references support `env:NAME`, `file:path`, and
  `keyring:service:account`. Relative file paths resolve from the configuration
  file's directory. Keyring access requires a working OS credential service.
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

### Maildir folder and flag preservation

Maildir targets keep the legacy flat layout when `preserve_folders` and
`preserve_flags` are omitted or false. New configurations can enable both; the
example configuration does so explicitly. Preserved folder labels are encoded
into safe directory components under `.folders`, with `/` retaining hierarchy.
The initial IMAP system flags map to Maildir filename flags (`Seen` to `S`,
`Answered` to `R`, `Flagged` to `F`, `Deleted` to `T`, and `Draft` to `D`). This
captures flags when a message is archived. Run `sync --refresh-flags` to refresh
flags for previously delivered IMAP messages without downloading their bodies.
This updates Maildir filenames when `preserve_flags` is enabled and updates the
integrity manifest for all targets. It never deletes backups when source mail is
deleted. Older receipts without a manifest need a repair run before flag refresh.
Custom IMAP keywords are retained in the manifest for restore; Maildir filenames
map the five standard system flags. Flagged messages are published in `cur/`. Windows cannot represent the standard `:2,` filename
suffix, so set `preserve_flags: false` on Windows. Switching preservation modes
creates a separate receipt scope and can write new copies for the new layout;
existing backup files are retained.

### S3 operation limits

S3 initialization and each message upload have a five-minute total timeout,
including SDK retries and response-body reads. Set `operation_timeout` in the
target's `config` (for example, `operation_timeout: "10m"`) for larger uploads.
The value must be positive; an earlier caller deadline still takes precedence.

S3 user metadata is independently limited to 2,048 UTF-8 key/value bytes.
Ordinary metadata remains unchanged. When it would exceed that budget, the
largest folder/UID/Message-ID/date values are replaced by `<field>-sha256`
metadata containing their hex digests until the budget is met. These digest
fields cannot reconstruct the original oversized values; raw RFC822 message
bytes and stable object keys are unaffected.

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
