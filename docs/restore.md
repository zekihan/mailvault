# Restore mail from a backup

`mailvault restore` copies messages from a configured backup target into one new IMAP mailbox. It keeps the original RFC 822 bytes and restores source flags and internal dates when the backup contains them. `\\Recent` is omitted because IMAP servers assign it. Restore never deletes, expunges, or rewrites backup messages.

Choose a new mailbox name explicitly. A mailbox that already exists is rejected for a new restore job. The same job can resume into its mailbox using the local restore receipts.

Preview and verify every backup message before connecting to the destination account:

```sh
mailvault restore --from archive --source account --folder 'Mailvault Restore 2026-10' --dry-run
```

Start or resume the restore by removing `--dry-run`:

```sh
mailvault restore --from archive --source account --folder 'Mailvault Restore 2026-10'
```

The `--source` value selects the configured IMAP account and its existing password or OAuth settings. `--from` selects a configured Maildir, mbox, or S3 target. The destination mailbox is a separate folder; the backup's original folder is retained as message metadata where the target supports it.

Restore receipts are stored separately from backup and sync receipts in the local state database. The job pins the destination mailbox's IMAP UIDVALIDITY generation; a resumed restore stops if that generation changed, disappeared, or is zero. This catches a mailbox deleted and recreated under the same name. Old restore jobs without a recorded generation fail closed; choose a new destination folder rather than reusing their receipts. Completed messages are skipped on resume by destination account, folder generation, backup storage target, and raw-message SHA-256, so identical bytes found through multiple backup folders or repaired locators are restored once. Before creating the mailbox, Mailvault records the restore job so a cancellation after mailbox creation can resume safely. It verifies every message hash before creating the mailbox, then reads and verifies each message again immediately before APPEND. Before each APPEND, Mailvault writes a pending marker; it clears that marker only after the server confirms APPEND and the completed receipt is saved. A pending marker means the server may have accepted that message even if the client did not receive confirmation. Mailvault stops that restore job on a later run until the destination has been inspected and the uncertain state is resolved, avoiding a silent duplicate.

After inspecting the destination mailbox, resolve one uncertain message by marking it already restored:

```sh
mailvault restore resolve --from archive --source account --folder 'Mailvault Restore 2026-10' --locator 'cur/message' --action mark-restored
```

If the message is absent, choose `--action retry` to let the next restore attempt APPEND it. Retrying can create a duplicate if the server accepted the earlier APPEND, so inspect first. `mark-restored` records completion without contacting IMAP.

If a connection failed while creating the new mailbox and Mailvault could not tell whether the server created it, inspect the destination and resolve setup state before retrying. Use `--action resume-setup` if that mailbox exists and belongs to this restore, or `--action retry-setup` only if it does not exist.

The dry run reads and hashes the selected backup messages but does not resolve destination credentials, connect to the destination account, or write restore receipts. A mismatch between a manifest hash and the message bytes stops the operation before APPEND. Legacy physical messages without a historical manifest hash are reported as unverified; a live restore requires `--allow-unverified` after reviewing the preview. The restore-time hash only checks consistency between its validation and APPEND reads; it does not establish historical integrity.
