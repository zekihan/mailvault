# Mailvault Helm chart

Kubernetes 1.27+ and a provisioner for persistent volumes are required. Proton is
optional and disabled by default. Mailvault runs as a CronJob; when enabled,
Proton Bridge runs continuously as a single Deployment with Recreate updates.
There are no proxies, host ports, or privileged containers. Scheduling defaults
to suspended and cannot be enabled without `initialized: true`.

Run the local-file examples from this repository checkout. TLS files generated
below live under ignored `secrets/`; the legacy
`proton-tls/` path is also ignored. Never force-add these files to Git.

The patched Bridge image `3.27.1-r1` must be published from `zekihan/dockerfiles`
before deploying Proton. Do not substitute the original 3.27.1 image: it only
listens on loopback. Select a published Mailvault image tag/digest in values;
The Mailvault image tag defaults to the semantic version in chart appVersion
(currently `0.2.1`). Ensure that image version has been published before installing,
or override it with an available tag/digest.

## Storage and configuration

The chart creates retained PVCs for `/state` (SQLite and OAuth token files) and
`/backups`. Proton adds a separate PVC mounted at `/root` for Bridge's session,
GPG keyring, and message cache. Configure sizes/storage classes in values.yaml,
or supply existing claims. Keep the release name and destination paths stable.
Do not share a Bridge PVC between releases. Back up the database and mail data
consistently; Helm retention is not a backup. Retained claims may require manual
adoption on reinstall; `existingClaim` is the explicit reuse path.

Put `config.yaml` and referenced credential files in an existing Secret:

```sh
kubectl create namespace mailvault
kubectl -n mailvault create secret generic mailvault-config \
  --from-file=config.yaml=/path/to/kubernetes-config.yaml \
  --from-file=mail-password=/path/to/password-file
```

Use `password_ref: file:/config/mail-password` in that configuration. Target
Maildir paths must be under `/backups`. For OAuth, use a persistent file token
store under `/state` rather than the desktop keyring, and initialize it before
scheduling. Do not put passwords or tokens in Helm values or commit them.

Without Proton:

```sh
helm upgrade --install backup ./charts/mailvault -n mailvault \
  --set image.tag=YOUR_PUBLISHED_TAG
```

This creates the suspended CronJob `backup-mailvault`. Provision credentials,
then follow the manual backup and scheduling steps below. No Bridge resources,
certificate mounts, or Proton settings are created when Proton is disabled.

## OAuth initialization (Mailvault)

For each OAuth source, configure `oauth2.token_file: /state/SOURCE-oauth.json`
with a distinct path, and omit `oauth2.token_key`. Keep the provider, client ID,
scopes, and mailbox username in config.yaml as in your working local OAuth
configuration. The file store creates private token files and refreshes them on
this writable PVC; do not put tokens on the read-only config Secret.

Suspend scheduling first:

```sh
helm upgrade backup ./charts/mailvault -n mailvault --reuse-values \
  --set suspend=true
kubectl -n mailvault get jobs
```

Wait for existing Mailvault Jobs to finish before the next command. Suspension
does not terminate a running Job. Then enable setup:

```sh
helm upgrade backup ./charts/mailvault -n mailvault --reuse-values \
  --set setup.enabled=true
kubectl -n mailvault rollout status deployment/backup-mailvault-setup --timeout=300s
kubectl -n mailvault exec -it deployment/backup-mailvault-setup -- \
  mailvault --config /config/config.yaml --state-dir /state auth login --source hotmail
```

Replace `hotmail` with the source name. Complete device authorization in your
browser using the same mailbox identity as the configured username. Providers
requiring a browser callback need the corresponding callback routing; these
commands cover device-code login. Repeat for each source, then disable setup:

```sh
helm upgrade backup ./charts/mailvault -n mailvault --reuse-values \
  --set setup.enabled=false
kubectl -n mailvault wait --for=delete pod \
  -l app.kubernetes.io/instance=backup,app.kubernetes.io/purpose=setup --timeout=300s
```

Only then run the manual backup below. Do not run Jobs alongside setup: both
mount the same state/backup claims, and token refresh can race with login.
The chart rejects unsuspending while Mailvault setup is enabled. The setup
Deployment has the same configuration, volumes, image, and CA trust as the Job,
and is allowed through the Proton ingress policy when Proton is enabled.

## Initial Proton setup

These examples use release `backup`, namespace `mailvault`, and Service DNS
`backup-mailvault-bridge.mailvault.svc`. Change them together if needed. Retain
these settings in a private values file for subsequent upgrades.

Generate a private CA and a Service certificate on your workstation (OpenSSL
1.1.1+). Alternatively, provide equivalent certificates from your PKI. Keep the
CA private key offline; it is not uploaded to Kubernetes.

```sh
mkdir -p secrets
chmod 700 secrets
mkdir -m 700 secrets/proton-tls
cd secrets/proton-tls
umask 077
openssl req -x509 -newkey rsa:3072 -nodes -days 3650 \
  -keyout ca.key -out ca.crt -subj '/CN=Mailvault Bridge CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
openssl req -newkey rsa:3072 -nodes -keyout tls.key -out tls.csr \
  -subj '/CN=backup-mailvault-bridge.mailvault.svc'
cat > server.ext <<'EXT'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:backup-mailvault-bridge.mailvault.svc
EXT
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 365 -out tls.crt -extfile server.ext
kubectl -n mailvault create secret generic mailvault-proton-tls \
  --from-file=tls.crt --from-file=tls.key --from-file=ca.crt
cd ../..
helm upgrade --install backup ./charts/mailvault -n mailvault \
  --set image.tag=YOUR_PUBLISHED_TAG --set proton.enabled=true
kubectl -n mailvault rollout status deployment/backup-mailvault-bridge-setup --timeout=300s
kubectl -n mailvault exec -it deployment/backup-mailvault-bridge-setup -- bash /setup/bridge.sh init
```

Inside Bridge:

1. Run `login` and complete authentication and 2FA.
2. Run `info`; save the generated Bridge username/password in your config Secret.
3. Run `cert import`, entering `/tls/tls.crt` and `/tls/tls.key`.
4. Run `cert export`, entering `/root/tls-export`, then `exit`.

Bridge stores the imported certificate **paths**. Both setup and runtime mount
these same paths; copying only the exported public certificate is insufficient.
The private server key is mounted only into Bridge, never into Mailvault.

Use the following source in your config Secret, adding a Maildir target under
`/backups` and the matching pair as in the repository's example configuration:

```yaml
sources:
  - name: proton
    type: imap
    host: backup-mailvault-bridge.mailvault.svc
    port: 1143
    username: YOUR_BRIDGE_USERNAME
    password_ref: file:/config/proton-password
    use_tls: false
    start_tls: true
    folders: [INBOX]
    max_connections: 1
```

Select the actual folders you want, including archived mail if desired; this
example covers Inbox only. Exclude Spam and Trash explicitly from your selection.
Mailvault trusts `ca.crt` via `SSL_CERT_FILE` and verifies the Service hostname.
The bind-address patch affects IMAP and SMTP; the Service and ingress policy
expose only IMAP. A NetworkPolicy-capable CNI is required to enforce that policy.

Finish setup while keeping scheduling suspended:

```sh
helm upgrade backup ./charts/mailvault -n mailvault --reuse-values \
  --set proton.setup=false
kubectl -n mailvault rollout status deployment/backup-mailvault-bridge --timeout=300s
```

Allow the initial mailbox synchronization to finish before the first backup.
TCP readiness only means IMAP is listening, not that Proton catch-up is complete.
Bridge stays running between scheduled backups and catches up incrementally.
Outages can still delay newly arrived messages until a later backup.

## Manual backup, then enable scheduling

Manual Jobs bypass CronJob suspension and its concurrency policy. Run only one
at a time and do not start one while a scheduled job is active. The application's
state lock also protects against concurrent syncs using the same state directory.

```sh
kubectl -n mailvault create job --from=cronjob/backup-mailvault backup-first
kubectl -n mailvault logs -f job/backup-first -c mailvault
kubectl -n mailvault wait --for=condition=Complete job/backup-first --timeout=14400s
helm upgrade backup ./charts/mailvault -n mailvault --reuse-values \
  --set initialized=true --set suspend=false
```

Inspect failed Jobs rather than enabling scheduling if the first backup fails.
The default schedule is daily at 03:00 UTC; configure `schedule` and `timeZone`.
Jobs have a four-hour deadline, no automatic retries, and `concurrencyPolicy:
Forbid`. Increase the deadline for a large first import. Preserve `initialized`
and `suspend` in your desired values after successful initialization.

## Reauthorization and certificate rotation

First suspend scheduling and wait for existing Jobs to finish (suspension does
not stop them). Scale the Bridge Deployment to zero and wait for its Pods to
terminate **before** enabling `proton.setup=true`; this avoids RWO volume
attachment conflicts. Keep `suspend=true` throughout maintenance. Never use the
same Bridge state concurrently from setup and runtime.

For certificate renewal, replace the certificate Secret with a matching key and
certificate for the same DNS name. If the issuing CA changes, replace `ca.crt`
too. Keep `/tls/tls.crt` and `/tls/tls.key` paths unchanged and restart Bridge;
new Jobs mount the updated CA. If re-import is needed, use setup mode and the
same CLI steps. Disable setup, verify a manual backup, then resume scheduling.
No certificate is trusted by fetching it from the network, and TLS verification
is never disabled. Protect both the Kubernetes Secrets and the Bridge PVC:
the unattended GPG key inside that volume permits decrypting its credentials.

## Published OCI chart

The release workflow packages and pushes the chart after checks and container
publication succeed for a `v*` release tag. The chart version, appVersion, and
default Mailvault image tag match that release (for example, `v1.2.3` becomes
`1.2.3`). SemVer build metadata stays in the chart version and appVersion but
is omitted from the default container tag, matching the release workflow
(`0.2.0-rc.1+build.2` uses image tag `0.2.0-rc.1`). Explicit `image.tag`
overrides are used unchanged. Manual branch runs publish containers only; they do not overwrite a
released chart. Authentication uses the workflow's GITHUB_TOKEN with packages
write permission. Make the GHCR chart package public if anonymous pulls are wanted.

Once a release is published:

```sh
helm upgrade --install backup oci://ghcr.io/zekihan/charts/mailvault \
  --version 1.2.3 --namespace mailvault --create-namespace \
  -f /path/to/private-values.yaml
```

Replace `1.2.3` with an actual published release. Initial scheduling remains
suspended; follow the setup instructions above before enabling it.
