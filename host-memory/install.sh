#!/bin/sh
set -eu

# Build-time source integration only. This is never a runtime wrapper or daemon.
if [ "$#" -ne 2 ]; then
 printf '%s\n' 'usage: install.sh <containerd-2.3.5-source> <kata-3.32.0-source>' >&2
 exit 2
fi
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerd=$(CDPATH= cd -- "$1" && pwd)
kata=$(CDPATH= cd -- "$2" && pwd)
module=github.com/SylphxAI/talos-images/host-memory/guard
[ "$(cat "$kata/VERSION")" = '3.32.0' ] || { printf '%s\n' 'Kata must be exactly 3.32.0' >&2; exit 1; }
grep -qx 'module github.com/containerd/containerd/v2' "$containerd/go.mod" || { printf '%s\n' 'containerd v2 source required' >&2; exit 1; }
grep -qx "module $module" "$root/guard/go.mod" && [ -f "$root/guard/guard.go" ] || { printf '%s\n' 'native guard module is missing' >&2; exit 1; }
# Every touched upstream file is pinned before mutation. No best-effort
# patching, reversed-patch detection or source-version fallback.
(cd "$containerd" && sha256sum -c "$root/patches/containerd-2.3.5-source.sha256")
(cd "$kata" && sha256sum -c "$root/patches/kata-3.32.0-source.sha256")
(cd "$containerd" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/containerd-2.3.5.patch")
# Preserve Talos's machined image-verifier integration on the new pin. This is
# the source-compatible port of Sidero's behavior, not its removal.
(cd "$containerd" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/containerd-2.3.5-talos-image-verify.patch")
(cd "$kata" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/kata-3.32.0.patch")
install_source() {
 [ ! -e "$2" ] || { printf '%s\n' "native source already exists: $2" >&2; exit 1; }
 cp "$1" "$2"
}
for source in "$root"/containerd/*.go; do
 install_source "$source" "$containerd/internal/cri/server/$(basename "$source")"
done
shim="$kata/src/runtime/pkg/containerd-shim-v2"
katautils="$kata/src/runtime/pkg/katautils"
install_source "$root/patches/kata-host-memory.go" "$shim/host_memory.go"
install_source "$root/patches/kata-host-memory_test.go" "$shim/host_memory_test.go"
install_source "$root/patches/kata-katautils-host-memory.go" "$katautils/host_memory.go"
install_source "$root/patches/kata-katautils-host-memory_test.go" "$katautils/host_memory_test.go"
install_source "$root/patches/kata-resourcecontrol-host-memory.go" "$kata/src/runtime/pkg/resourcecontrol/host_memory.go"
# The CRI server imports the single shared Guard module. containerd builds with
# -mod=vendor, so refresh vendor after adding the local replacement.
(cd "$containerd" && go mod edit -require="$module@v0.0.0" -replace="$module=$root/guard" && go mod vendor)
