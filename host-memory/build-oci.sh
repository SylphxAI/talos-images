#!/usr/bin/env bash
# Native stack build: no node operation, registry key or runtime wrapper.
set -euo pipefail
kind=${1:?expected kata, kubelet or imager}
case "$kind" in kata|kubelet|imager) ;; *) echo 'expected kata, kubelet or imager' >&2; exit 2 ;; esac
: "${OCI_DIR:?OCI output directory is required}"
: "${SOURCE_SHA:?immutable source revision is required}"
if [[ ! "$SOURCE_SHA" =~ ^[a-f0-9]{40}$ ]]; then echo 'full source revision required' >&2; exit 2; fi
root=$(git rev-parse --show-toplevel)
source_epoch=$(git show -s --format=%ct "$SOURCE_SHA")
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
native_dockerfile="$root/host-memory/Dockerfile"
build_native() {
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    --file "$native_dockerfile" --target "$1" --build-arg "SOURCE_REVISION=$SOURCE_SHA" \
    --output "type=oci,dest=$2" "$root"
}
if [ "$kind" = kata ]; then
  build_native kata-extension "$temporary/output.tar"
elif [ "$kind" = kubelet ]; then
  build_native kubelet-image "$temporary/output.tar"
else
  build_native containerd-package "$temporary/containerd.tar"
  mkdir "$temporary/containerd"
  tar -xf "$temporary/containerd.tar" -C "$temporary/containerd"
  curl -fsSL --max-time 120 https://codeload.github.com/siderolabs/talos/tar.gz/refs/tags/v1.14.1 -o "$temporary/talos.tar.gz"
  printf '%s  %s\n' 49710f8a98f9c98f88453cc2d5ebbfe5c128183785684a9defbc60ad5c76dd55 "$temporary/talos.tar.gz" | sha256sum -c -
  tar -xzf "$temporary/talos.tar.gz" -C "$temporary"
  # Use upstream's complete build arguments and core boot-asset graph. A local
  # OCI named context supplies the native package without an intermediate push
  # or a credential for another artifact repository.
  make -C "$temporary/talos-1.14.1" target-imager PLATFORM=linux/amd64 \
    TAG=v1.14.1 ABBREV_TAG=v1.14.1 "SHA=$SOURCE_SHA" "SOURCE_DATE_EPOCH=$source_epoch" \
    PKG_CONTAINERD=native-containerd PUSH=false \
    "TARGET_ARGS=--build-context=native-containerd=oci-layout://$temporary/containerd --label=org.opencontainers.image.source=https://github.com/SylphxAI/talos-images --label=org.opencontainers.image.revision=$SOURCE_SHA --output=type=oci,dest=$temporary/output.tar"
fi
mkdir -p "$OCI_DIR"
tar -xf "$temporary/output.tar" -C "$OCI_DIR"
