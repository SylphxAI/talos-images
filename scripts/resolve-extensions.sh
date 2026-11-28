#!/bin/bash
set -euo pipefail

if [ "${UKI_NESTED_KVM:-false}" = true ]; then
  source scripts/stock-nested-profile.sh
  # Only the build step can supply the verified extension digest.
  kata_ref=${STOCK_NESTED_KATA_REF:-}
  if [[ "$kata_ref" != "$STOCK_NESTED_REPO"@* || ! "${kata_ref#*@}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo '::error::the stock nested UKI profile requires its built Kata digest' >&2
    exit 1
  fi
  {
    printf '%s\n' "ref=$kata_ref" 'stargz_ref=' 'gvisor_ref=' 'kubelet_ref='
    printf '%s\n' "imager_ref=$STOCK_NESTED_IMAGER" "artifact_tag=$STOCK_NESTED_TAG" 'make_latest=false'
    printf 'flags= --system-extension-image %s --base-installer-image %s --extra-kernel-arg kvm_amd.nested=1\n' \
      "$kata_ref" "$STOCK_NESTED_INSTALLER_BASE"
  } >> "$GITHUB_OUTPUT"
  exit 0
fi

IMAGER_REF="ghcr.io/siderolabs/imager:${TALOS_VERSION}"
NATIVE_TAG=""
KUBELET_REF=""
if [ -n "${NATIVE_RUNTIME_RELEASE:-}" ]; then
  if [ -n "${KATA_EXTENSION_IMAGE:-}" ]; then
    echo "::error::native runtime release cannot be mixed with a separate Kata override" >&2
    exit 1
  fi
  RELEASE_JSON=$(python3 scripts/runtime-release.py)
  IMAGER_REF=$(jq -r '.imager' <<< "$RELEASE_JSON")
  KATA_EXTENSION_IMAGE=$(jq -r '.kata_extension' <<< "$RELEASE_JSON")
  NATIVE_TAG=$(jq -r '.artifact_tag' <<< "$RELEASE_JSON")
  KUBELET_REF=$(jq -r '.kubelet' <<< "$RELEASE_JSON")
fi

CATALOG="https://factory.talos.dev/version/${TALOS_VERSION}/extensions/official"
CATALOG_JSON=""
if [ -z "${KATA_EXTENSION_IMAGE:-}" ] || [ "${STARGZ_ENABLED:-false}" = true ] || [ "${GVISOR_ENABLED:-false}" = true ]; then
  CATALOG_JSON=$(curl -fsSL "${CATALOG}")
fi

catalog_ref() {
  local name=$1 ref
  ref=$(jq -r --arg name "$name" '
    [.[] | select(.name == $name) | "\(.ref)@\(.digest)"][0] // empty
  ' <<< "$CATALOG_JSON")
  if [[ ! "$ref" =~ ^[a-z0-9./_-]+:[a-zA-Z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "::error::no ${name} with a valid pinned reference in the factory catalog for ${TALOS_VERSION} (${CATALOG})" >&2
    return 1
  fi
  printf '%s' "$ref"
}

KATA_REF=${KATA_EXTENSION_IMAGE:-}
if [ -z "$KATA_REF" ]; then
  KATA_REF=$(catalog_ref siderolabs/kata-containers)
fi
FLAGS=""
while IFS= read -r ext; do
  [ -z "$ext" ] && continue
  FLAGS="${FLAGS} --system-extension-image ${ext}"
done < <(sed -n 's/^ *- //p' extensions.yaml)
FLAGS="${FLAGS} --system-extension-image ${KATA_REF}"

STARGZ_REF=""
ARTIFACT_TAG=${TALOS_VERSION}
MAKE_LATEST=true
if [ -n "$NATIVE_TAG" ]; then
  ARTIFACT_TAG="$NATIVE_TAG"
  MAKE_LATEST=false
fi
if [ "${STARGZ_ENABLED:-false}" = true ]; then
  STARGZ_REF=$(catalog_ref siderolabs/stargz-snapshotter)
  FLAGS="${FLAGS} --system-extension-image ${STARGZ_REF}"
  ARTIFACT_TAG="${ARTIFACT_TAG}-stargz"
  MAKE_LATEST=false
fi

GVISOR_REF=""
if [ "${GVISOR_ENABLED:-false}" = true ]; then
  GVISOR_REF=$(catalog_ref siderolabs/gvisor)
  FLAGS="${FLAGS} --system-extension-image ${GVISOR_REF}"
  ARTIFACT_TAG="${ARTIFACT_TAG}-gvisor"
  MAKE_LATEST=false
fi

{
  echo "ref=${KATA_REF}"
  echo "stargz_ref=${STARGZ_REF}"
  echo "gvisor_ref=${GVISOR_REF}"
  echo "imager_ref=${IMAGER_REF}"
  echo "kubelet_ref=${KUBELET_REF}"
  echo "flags=${FLAGS}"
  echo "artifact_tag=${ARTIFACT_TAG}"
  echo "make_latest=${MAKE_LATEST}"
} >> "$GITHUB_OUTPUT"
echo "Extensions:${FLAGS}"
echo "Artifact tag: ${ARTIFACT_TAG}"
