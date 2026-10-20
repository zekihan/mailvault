#!/bin/bash
set -euo pipefail
umask 077

exec 9>/root/.mailvault-bridge.lock
if ! flock -n 9; then
  echo "Bridge state is already in use. Stop the bridge service before initialization." >&2
  exit 1
fi

check_keyring() {
  local probe="mailvault-keyring-check-$$"
  if ! printf '%s\n' 'mailvault-keyring-check' | pass insert --multiline "$probe" >/dev/null ||
     [[ "$(pass show "$probe")" != mailvault-keyring-check ]]; then
    echo 'Bridge keyring is not usable; refusing to start without encrypted credential storage.' >&2
    exit 1
  fi
  pass rm --force "$probe" >/dev/null
}

case "${1:-}" in
  init)
    # Reuse the keyring when reauthorizing or exporting a rotated certificate.
    if [[ ! -f /root/.password-store/.gpg-id ]]; then
      gpg --batch --passphrase "" --quick-generate-key pass-key rsa2048 encr 0
      pass init pass-key
    fi
    check_keyring
    mkdir -p /root/tls-export /bridge-certs
    echo 'In Bridge, run: login, info, cert export (path /root/tls-export), then exit.'
    /protonmail/proton-bridge --cli
    if [[ ! -s /root/tls-export/cert.pem ]]; then
      echo 'Certificate not exported. Run initialization again and use cert export.' >&2
      exit 1
    fi
    # Only the public certificate crosses into the client trust volume.
    cp /root/tls-export/cert.pem /bridge-certs/cert.pem.tmp
    chmod 644 /bridge-certs/cert.pem.tmp
    mv /bridge-certs/cert.pem.tmp /bridge-certs/cert.pem
    echo 'Bridge public certificate copied to the client trust volume.'
    ;;
  serve)
    if [[ ! -s /root/.password-store/.gpg-id || ! -s /bridge-certs/cert.pem ]]; then
      echo 'Initialize Bridge first: docker compose -f compose.proton.yaml run --rm bridge-init' >&2
      exit 1
    fi
    check_keyring
    exec /protonmail/proton-bridge --noninteractive
    ;;
  *) echo 'Expected init or serve' >&2; exit 2 ;;
esac
