#!/bin/bash
set -euo pipefail

# Only this opt-in can use the reviewed fixed native + stargz baseline.
if [ "${UKI_NESTED_KVM:-false}" = true ]; then
  if [ "$TALOS_VERSION" != v1.14.1 ] || [ -n "${NATIVE_RUNTIME_RELEASE:-}${KATA_EXTENSION_IMAGE:-}" ] ||
     [ "${NATIVE_HOST_MEMORY:-false}" = true ] || [ "${STARGZ_ENABLED:-false}" = true ]; then
    echo '::error::the nested UKI profile requires Talos v1.14.1 with no native, Kata or stargz overrides' >&2
    exit 1
  fi
  NATIVE_RUNTIME_RELEASE=$(cat profiles/v1.14.1-d34956ba-native-runtime.json)
  export NATIVE_RUNTIME_RELEASE
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
if [ -z "${KATA_EXTENSION_IMAGE:-}" ] || [ "${STARGZ_ENABLED:-false}" = true ]; then
  CATALOG_JSON=$(curl -fsSL "${CATALOG}")
fi

catalog_ref() {
  local name=$1 ref
  ref=$(jq -r --arg name "$name" '
    [.[] | select(.name == $name) | "\(.ref)@\(.digest)"][0] // empty
  ' <<< "$CATALOG_JSON")
  if [ -z "$ref" ] || [ "$ref" = "@" ]; then
    echo "::error::no ${name} in the factory catalog for ${TALOS_VERSION} (${CATALOG})" >&2
    return 1
  fi
  printf '%s' "$ref"
}

KATA_REF=${KATA_EXTENSION_IMAGE:-}
if [ -z "$KATA_REF" ]; then
  KATA_REF=$(catalog_ref siderolabs/kata-containers)
fi
FLAGS=""
if [ "${UKI_NESTED_KVM:-false}" != true ]; then
  while IFS= read -r ext; do
    [ -z "$ext" ] && continue
    FLAGS="${FLAGS} --system-extension-image ${ext}"
  done < <(sed -n 's/^ *- //p' extensions.yaml)
fi
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

if [ "${UKI_NESTED_KVM:-false}" = true ]; then
  STARGZ_REF=ghcr.io/siderolabs/stargz-snapshotter:v0.18.2@sha256:b5ed92e4620173c6c5f8b883bf8bcee2dd828e15918b6f166382abac409f8c63
  FLAGS="${FLAGS} --system-extension-image ${STARGZ_REF} --extra-kernel-arg kvm_amd.nested=1"
  # Each run attempt gets a fresh tag rather than replacing an existing artifact.
  : "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
  ARTIFACT_TAG="$ARTIFACT_TAG-stargz-uki-nested-kvm-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
fi

{
  if [ "${UKI_NESTED_KVM:-false}" = true ]; then
    printf 'native_manifest=%s\n' "$(jq -c 'del(.artifact_tag)' <<< "$RELEASE_JSON")"
  fi
  echo "ref=${KATA_REF}"
  echo "stargz_ref=${STARGZ_REF}"
  echo "imager_ref=${IMAGER_REF}"
  echo "kubelet_ref=${KUBELET_REF}"
  echo "flags=${FLAGS}"
  echo "artifact_tag=${ARTIFACT_TAG}"
  echo "make_latest=${MAKE_LATEST}"
} >> "$GITHUB_OUTPUT"
echo "Extensions:${FLAGS}"
echo "Artifact tag: ${ARTIFACT_TAG}"
