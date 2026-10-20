# Proton Bridge and Mailvault with Docker Compose

Requires Docker Compose v2.24+ and a paid Proton plan that includes Mail.
Run commands from the repository root. This setup is separate from `config.yaml`
and existing local backups. The default test backs up **Inbox only**; edit
`deploy/proton/mailvault.yaml` to include the exact archive, sent, drafts, and
custom folder names you want. Keep Spam and Trash out of that list.

Bridge is built from Proton's v3.27.1 release commit, not a third-party binary
image. Both images support native builds on ARM64 and AMD64. The first build
compiles Bridge and may take several minutes.

## Build and initialize

```sh
docker compose -f compose.proton.yaml build bridge mailvault
docker compose -f compose.proton.yaml run --rm bridge-init
```

At the Bridge prompt, run these commands one at a time:

```text
login
info
cert export
```

Complete login and 2FA yourself. `info` shows the IMAP username and generated
Bridge password; save those locally for the next step. Keep Bridge's default
IMAP port **1143** and **STARTTLS** mode for this configuration.

When `cert export` asks for a directory, enter:

```text
/root/tls-export
```

Then enter `exit` and let the container finish normally. The wrapper copies only
`cert.pem` into the shared certificate volume. The private key stays in Bridge's
private state volume. Do not interrupt the wrapper before it prints its
certificate-copy confirmation.

## Configure the client credentials

```sh
cp deploy/proton/env.example .env.proton
mkdir -p secrets
chmod 700 secrets
```

Edit `.env.proton`: set `MAILVAULT_SOURCES_0_USERNAME` to the username shown by
Bridge `info`. Enter the **Bridge-generated password**, not the Proton account
password, using zsh on macOS:

```zsh
read -rs 'proton_bridge_password?Bridge password: '
(umask 077; printf '%s' "$proton_bridge_password" > secrets/proton-bridge-password)
unset proton_bridge_password
echo
```

Both credential files are ignored by Git and excluded from the Mailvault build
context. The password is mounted as a Compose secret, not placed in the image
or passed on a command line. Local Compose secrets are backed by the host file;
protect that file as a credential.

## Validate and sync

```sh
docker compose -f compose.proton.yaml up -d --wait bridge
docker compose -f compose.proton.yaml run --rm mailvault validate-config
docker compose -f compose.proton.yaml run --rm mailvault sync
```

Repeat the last command for incremental runs. `mailvault` is a one-shot service
in the `tools` profile, so `up -d` alone does not initiate a backup. Use `run`
without `--no-deps` to wait for Bridge's health check. An open IMAP socket signals
server availability; account access is checked by Mailvault itself.

Verification and repair:

```sh
docker compose -f compose.proton.yaml run --rm mailvault verify proton_maildir
docker compose -f compose.proton.yaml run --rm mailvault repair --source proton --target proton_maildir --apply
```

A repair fetches source messages to compare/adopt existing backups; use ordinary
`sync` for routine incremental work. These commands select only Proton.

## Persistence and shutdown

The fixed Compose project name is `mailvault-proton`. Named volumes retain:

| Volume | Contents |
| --- | --- |
| `mailvault-proton_bridge-state` | Proton session, GPG/pass keyring, cache, private TLS key |
| `mailvault-proton_bridge-certs` | Public certificate trusted by Mailvault |
| `mailvault-proton_mailvault-state` | `/state/state.db` and incremental receipts |
| `mailvault-proton_mailvault-backups` | Maildir at `/backups/proton` |

```sh
docker compose -f compose.proton.yaml down
```

Normal `down` retains the volumes. **Do not add `--volumes` / `-v`** unless you
intend to delete these backups and credentials. Keep the project name, state
mount, and `/backups/proton` destination stable across runs. Back up the state DB
with the Maildir. For a consistent filesystem copy, stop Mailvault first.

Bridge uses an unattended GPG/pass keyring inside its private volume. Anyone
with access to that volume can recover its credentials; it is not a substitute
for encrypted disks and access controls. Mailvault never mounts that volume.
A lock prevents initialization and a running Bridge from using it concurrently.

## Certificate trust

No host ports are published. Mailvault shares Bridge's network namespace and
connects to `127.0.0.1:1143`. This preserves the hostname/IP expected by Bridge's
certificate and avoids exposing decrypted IMAP traffic to the host or LAN.

Mailvault sets `SSL_CERT_FILE=/bridge-certs/cert.pem` inside its Linux container.
Go's normal TLS verification checks trust, validity, and the server IP. There is
no `InsecureSkipVerify`, automatic trust from a network connection, or host
Keychain modification. STARTTLS must succeed before credentials are sent.

Missing or expired certificates fail with setup guidance. If Bridge regenerates
its certificate, stop Bridge, export the new certificate from its own CLI, and
restart:

```sh
docker compose -f compose.proton.yaml stop bridge
docker compose -f compose.proton.yaml run --rm bridge-init
# In the CLI: cert export -> /root/tls-export -> exit
docker compose -f compose.proton.yaml up -d --wait bridge
```

Initialization reuses the existing keyring. Only run `login` again if necessary.
If you change Bridge's IMAP port or security mode, update the source config and
health check accordingly. Do not disable certificate verification to hide a
configuration mismatch.

## Later Kubernetes migration

Reuse this image pair with Bridge as a native sidecar, persistent volumes for
Bridge and Mailvault, a Secret for the Bridge password, and a read-only public
certificate mount. Preserve the same in-container paths and loopback connection.
Use `concurrencyPolicy: Forbid`, and complete the interactive Bridge login once
against its PVC before enabling the CronJob. This Compose file does not deploy
anything to Kubernetes.
