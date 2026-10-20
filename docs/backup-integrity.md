# Backup verification and repair

`mailvault verify` checks the stored messages for a configured target against the SHA-256 hashes recorded in Mailvault's state database. It reads target data and state without creating or migrating the database. It does not contact mail sources.

```sh
mailvault verify <target> [--config <path>] [--state-dir <directory>] [--json]
```

The command reports scanned, verified, corrupt, missing, and unverifiable entries. It exits successfully only when no unresolved entry is found. JSON output is still written before a nonzero exit when verification finds a problem.

To repair one configured source-to-target pair, fetch the current source messages and pass `--apply`:

```sh
mailvault repair --source <source> --target <target> --apply \
  [--config <path>] [--state-dir <directory>] [--json]
```

Repair requires `--apply`; there is no separate repair dry-run flag. Run `verify` first to preview the target's current integrity. Repair writes corrected messages to the selected target, then checks that the requested source-target manifest is healthy. Messages that are missing from the source cannot be reconstructed and remain unresolved.

Maildir, mbox, and S3 targets support verification and repair. The target must be readable for verification and writable for repair. S3 verification lists and reads objects; it does not create a bucket. Repair stores a replacement as a new object or message, retaining damaged data for investigation.

## State and legacy backups

The state database, normally `~/.local/share/mailvault/state.db`, contains the manifest that links each source, scope, folder, and UID to its target locator, raw-message hash, flags, and internal date. Keep this database with the backup. If it is removed or replaced, Mailvault loses the durable source-to-object mapping and cannot fully verify or restore backups from it.

Receipts, manifests, and repair history are bound to the selected target's storage location. Maildir and mbox use their canonical absolute paths; S3 uses its bucket, region, endpoint, and prefix. Changing a target's location makes Mailvault ignore records created for its previous location. The next sync writes messages to the new location, while the old files and records remain available. Legacy records without a proven location are not treated as evidence for the current target, so the first sync to that location performs a one-time delivery scan.

Only delivery receipts and manifests scoped to the current storage location can establish what is stored there. State from before location identities existed has no reliable destination binding, so its receipts and manifests are retained but not used as proof for the current location. Existing physical backups without a location-scoped manifest remain unverifiable until a repair run adopts and checks them against current source messages or a fresh sync writes them again. That first sync can create duplicate Maildir/mbox copies or additional S3 objects; later runs use the new location-scoped receipts.

When repair finds a damaged object, it keeps the original object and records it as superseded in the state database. The repaired copy becomes the active manifest location; the damaged original remains on disk or in the bucket for inspection. Preserve the state database so verification can distinguish that retained copy from an active backup.

Verification can identify missing or changed objects represented in the manifest. It cannot prove that a source message still exists unless a repair run fetches and checks that source-target pair. A successful historical receipt alone is not evidence that the current target bytes are intact.

Readers currently limit individual messages to 512 MiB during verification and restore. An oversized message fails explicitly rather than being skipped or counted as verified.

An mbox file must begin with a valid `From ` separator. If that initial separator is missing or damaged, verification fails closed, including when no manifest exists; repair cannot safely identify and replace messages in that file until its structure is restored.
