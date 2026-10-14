#!/usr/bin/env bash
# Build-time source integration; never a wrapper or live node operation.
set -euo pipefail
[ "$#" = 1 ] || { echo 'usage: install-kubelet.sh <kubernetes-1.36.4-source>' >&2; exit 2; }
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
kubernetes=$(CDPATH= cd -- "$1" && pwd)
(cd "$kubernetes" && sha256sum -c "$root/patches/kubelet-1.36.4-source.sha256")
(cd "$kubernetes" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/kubelet-1.36.4.patch")
for source in "$root"/kubelet/*.go; do
  target="$kubernetes/pkg/kubelet/eviction/$(basename "$source")"
  [ ! -e "$target" ] || { echo "native kubelet source already exists: $target" >&2; exit 1; }
  cp "$source" "$target"
done
