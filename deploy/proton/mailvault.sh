#!/bin/sh
set -eu
: "${MAILVAULT_SOURCES_0_USERNAME:?Set MAILVAULT_SOURCES_0_USERNAME in .env.proton to the username shown by Bridge info}"
if [ ! -s /bridge-certs/cert.pem ]; then
  echo 'Missing Bridge certificate. Initialize Bridge and export its certificate first.' >&2
  exit 1
fi
if ! openssl x509 -in /bridge-certs/cert.pem -noout -checkend 0 >/dev/null 2>&1; then
  echo 'Bridge certificate is invalid or expired; export the current certificate using bridge-init.' >&2
  exit 1
fi
if [ ! -s /run/secrets/bridge-password ]; then
  echo 'Missing Bridge-generated password in secrets/proton-bridge-password.' >&2
  exit 1
fi
exec mailvault --config /config/mailvault.yaml --state-dir /state "$@"
