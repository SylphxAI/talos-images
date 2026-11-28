#!/usr/bin/env bash
# Closed composition for the stock nested-KVM dispatch.
if [[ "$TALOS_VERSION" != v1.14.1 || -n "${NATIVE_RUNTIME_RELEASE:-}${KATA_EXTENSION_IMAGE:-}" ||
      "${NATIVE_HOST_MEMORY:-false}" == true || "${STARGZ_ENABLED:-false}" == true || "${GVISOR_ENABLED:-false}" == true ]]; then
  echo '::error::the stock nested UKI profile requires Talos v1.14.1 with no native, Kata, stargz or gVisor overrides' >&2
  return 1
fi
if [[ ! "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ || ! "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]*$ ]]; then
  echo '::error::the stock nested UKI profile requires a numeric run ID and attempt' >&2
  return 1
fi
# Published linux/amd64 manifests of the upstream v1.14.1 and 3.32.0 tags.
STOCK_NESTED_IMAGER=ghcr.io/siderolabs/imager@sha256:bb74a5c4fc190997cca0930004978203f1e2e2cd637f4db1f26ef3b2eaef8338
STOCK_NESTED_INSTALLER_BASE=ghcr.io/siderolabs/installer-base@sha256:816b11f0bc34b7bf7b34e4fe5b3764c3056d7f66f8098559f8ca750b37d0948d
STOCK_NESTED_KATA_BASE=ghcr.io/siderolabs/kata-containers@sha256:cbc2653c8366ae56e69254a7db4b6f71d576b5371467eb8933c2cd053cbc6b21
STOCK_NESTED_TAG="v1.14.1-stock-uki-nested-kvm-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
STOCK_NESTED_REPO=ghcr.io/sylphxai/talos-installer
STOCK_NESTED_KATA_TARGET="$STOCK_NESTED_REPO:kata-$STOCK_NESTED_TAG"
