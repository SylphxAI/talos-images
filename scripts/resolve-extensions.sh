#!/bin/bash
set -euo pipefail

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
while IFS= read -r ext; do
  [ -z "$ext" ] && continue
  FLAGS="${FLAGS} --system-extension-image ${ext}"
done < <(sed -n 's/^ *- //p' extensions.yaml)
FLAGS="${FLAGS} --system-extension-image ${KATA_REF}"

STARGZ_REF=""
ARTIFACT_TAG=${TALOS_VERSION}
MAKE_LATEST=true
if [ "${STARGZ_ENABLED:-false}" = true ]; then
  STARGZ_REF=$(catalog_ref siderolabs/stargz-snapshotter)
  FLAGS="${FLAGS} --system-extension-image ${STARGZ_REF}"
  ARTIFACT_TAG="${TALOS_VERSION}-stargz"
  MAKE_LATEST=false
fi

{
  echo "ref=${KATA_REF}"
  echo "stargz_ref=${STARGZ_REF}"
  echo "flags=${FLAGS}"
  echo "artifact_tag=${ARTIFACT_TAG}"
  echo "make_latest=${MAKE_LATEST}"
} >> "$GITHUB_OUTPUT"
echo "Extensions:${FLAGS}"
echo "Artifact tag: ${ARTIFACT_TAG}"
