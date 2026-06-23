#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Codesign a darwin binary with Developer ID + hardened runtime using rcodesign.
# No-ops for non-darwin targets or when signing material is absent (snapshots).
set -euo pipefail

BIN_PATH="$1"
TARGET="$2" # e.g. darwin_arm64, linux_amd64

case "$TARGET" in
  darwin_*) ;;
  *) echo "macos-sign: skip non-darwin target $TARGET"; exit 0 ;;
esac

if [[ -z "${MACOS_CERT_P12:-}" || ! -s "${MACOS_CERT_P12}" ]]; then
  echo "macos-sign: no signing cert present, skipping"
  exit 0
fi

rcodesign sign \
  --p12-file "$MACOS_CERT_P12" \
  --p12-password-file "$MACOS_CERT_PASSWORD_FILE" \
  --code-signature-flags runtime \
  "$BIN_PATH"

echo "macos-sign: signed $BIN_PATH ($TARGET)"
