#!/usr/bin/env bash
# Closed amd64 compositions for the stock nested-KVM dispatch.
if [[ -n "${NATIVE_RUNTIME_RELEASE:-}${KATA_EXTENSION_IMAGE:-}" ||
      "${NATIVE_HOST_MEMORY:-false}" != false || "${STARGZ_ENABLED:-false}" != false || "${GVISOR_ENABLED:-false}" != false ]]; then
  echo '::error::the stock nested UKI profile does not accept native, Kata, stargz or gVisor overrides' >&2
  return 1
fi
if [[ ! "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ || ! "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]*$ ]]; then
  echo '::error::the stock nested UKI profile requires a numeric run ID and attempt' >&2
  return 1
fi
STOCK_NESTED_STARGZ=
STOCK_NESTED_GVISOR=
STOCK_NESTED_FACTORY_BASELINE=
STOCK_NESTED_TAG="$TALOS_VERSION-stock-uki-nested-kvm"
case "$TALOS_VERSION" in
  v1.14.1)
    STOCK_NESTED_IMAGER=ghcr.io/siderolabs/imager@sha256:bb74a5c4fc190997cca0930004978203f1e2e2cd637f4db1f26ef3b2eaef8338
    STOCK_NESTED_INSTALLER_BASE=ghcr.io/siderolabs/installer-base@sha256:816b11f0bc34b7bf7b34e4fe5b3764c3056d7f66f8098559f8ca750b37d0948d
    ;;
  v1.14.2)
    # FR-1 Factory schematic: preserve its exact three official extensions,
    # replacing only the guest kernel in Kata. All pins are linux/amd64.
    STOCK_NESTED_IMAGER=ghcr.io/siderolabs/imager@sha256:a09ccdeeffd8e5b5cce3d4a23599dfd527ff9043242a18c1bff45f469cec3cbe
    STOCK_NESTED_INSTALLER_BASE=ghcr.io/siderolabs/installer-base@sha256:90472c8dac7a0d79742951057c0bdd26e8f0e0027d837c48723666b6b6d50d61
    STOCK_NESTED_STARGZ=ghcr.io/siderolabs/stargz-snapshotter@sha256:be40f902374211bc841682e5899f6268b2529c61f7fc56f187b2e0cd8e34342a
    STOCK_NESTED_GVISOR=ghcr.io/siderolabs/gvisor@sha256:93fd4332e0ae8c9f4a51fe8590eba16a1ffc282bac836ceaae75e6fbbfddc2d7
    STOCK_NESTED_FACTORY_BASELINE=factory.talos.dev/metal-installer/a35b5a30b5802a629e3e13fca2d8c75de6133219c292573f3201b846da223519@sha256:4e7fcbec538c51e606ed47726c26f77a083c0a06c76056cb626b4745ecf7c0cd
    STOCK_NESTED_TAG+=-fr1
    ;;
  *)
    echo '::error::the stock nested UKI profile supports only Talos v1.14.1 or v1.14.2' >&2
    return 1
    ;;
esac
STOCK_NESTED_KATA_BASE=ghcr.io/siderolabs/kata-containers@sha256:cbc2653c8366ae56e69254a7db4b6f71d576b5371467eb8933c2cd053cbc6b21
STOCK_NESTED_TAG+="-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
STOCK_NESTED_REPO=ghcr.io/sylphxai/talos-installer
STOCK_NESTED_KATA_TARGET="$STOCK_NESTED_REPO:kata-$STOCK_NESTED_TAG"
