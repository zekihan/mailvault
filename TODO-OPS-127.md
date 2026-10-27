# OPS-127: remaining operational acceptance

The code and synthetic tests address contention within one process and bounded
external SQLite locks. Production acceptance is **not complete**. No live state,
credentials, access settings, deployment, or external messages were changed.

- [ ] Identify the image digest and build revision used by failed pod
  `mailvault/mailvault-mailvault-29881260-47xst` from retained pod/job metadata,
  logs, and image provenance. Do not infer this from a chart version.
- [ ] Record the currently deployed Mailvault image digest/build revision and
  actual state-volume filesystem, driver, mount options, and SQLite locking
  support. Confirm rollback journaling is appropriate for that volume.
- [ ] After separately authorized release/deployment, prove the deployed digest
  contains this fix and collect subsequent scheduled-run summaries for all three
  source-target pairs (fetched/written/skipped/error totals and folder-state
  errors). One successful run alone cannot establish an intermittent fix.
- [ ] If lock failures persist, identify competing processes and their lock
  duration without deleting locks/state or weakening receipt durability.

These checks need live metadata/log access and an authorized fix-containing
release/deployment. Deployment and live backup reruns are outside this task's
permission scope. Failed/deployed digests and scheduled-run outcomes remain
unverified. All local regression tests use synthetic temporary databases and
fixture sources/targets, with no mail accounts or real delivery.

Native Windows runtime checks also require a Windows runner; cross-compilation
on macOS does not replace them. GitHub Actions must not be retried or billing
changed if the private-repository Actions budget is exhausted.
