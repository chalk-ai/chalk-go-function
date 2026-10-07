#!/usr/bin/env bash
set -euo pipefail

# Source this file from Buildkite steps so the PATH update persists. go.mod's
# go directive is the minimum; GOTOOLCHAIN=auto fetches a newer one if needed.
GO_VERSION="${GO_VERSION:-1.26.8}"
GO_INSTALL_DIR="${GO_INSTALL_DIR:-/tmp/go-${GO_VERSION}}"
export PATH="${GO_INSTALL_DIR}/go/bin:${HOME}/go/bin:${PATH}"

current_version="$(go env GOVERSION 2>/dev/null || true)"
if [ "${current_version}" != "go${GO_VERSION}" ]; then
  rm -rf "${GO_INSTALL_DIR}"
  mkdir -p "${GO_INSTALL_DIR}"
  curl --proto '=https' --tlsv1.2 -fsSL --retry 4 \
    "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -xz -C "${GO_INSTALL_DIR}"
fi

go version
