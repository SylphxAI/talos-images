#!/usr/bin/env bash
# Build-time source integration only; never run against a node filesystem.
set -euo pipefail
[[ $# = 1 ]] || { echo 'usage: install.sh <pristine-talos-v1.14.1-source>' >&2; exit 2; }
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
target=$(CDPATH= cd -- "$1" && pwd)
declare -A overlays=(
  [inventory.go]=pkg/machinery/bootinventory/inventory.go
  [inventory_test.go]=pkg/machinery/bootinventory/inventory_test.go
  [talos/v1alpha1_efi_inventory.go]=internal/app/machined/internal/server/v1alpha1/efi_inventory.go
  [talos/v1alpha1_efi_inventory_test.go]=internal/app/machined/internal/server/v1alpha1/efi_inventory_test.go
  [talos/client_efi_inventory.go]=pkg/machinery/client/efi_inventory.go
  [talos/command_efi_inventory.go]=cmd/talosctl/cmd/talos/efi_inventory.go
  [talos/machine_efi_inventory_test.go]=pkg/machinery/api/machine/efi_inventory_test.go
)
# Check every boundary before changing any source. Existing/partial integration
# is an error, not a patch retry or silent replacement.
for source in "${!overlays[@]}"; do
  [[ -f "$root/$source" && ! -e "$target/${overlays[$source]}" && ! -L "$target/${overlays[$source]}" ]] || {
    echo 'EFI source overlay missing or destination already exists' >&2; exit 1;
  }
done
(cd "$target" && sha256sum --check "$root/patches/talos-1.14.1-source.sha256")
(cd "$target" && patch --dry-run --batch --forward --fuzz=0 -p1 < "$root/patches/talos-1.14.1.patch")
(cd "$target" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/talos-1.14.1.patch")
for source in "${!overlays[@]}"; do
  mkdir -p -- "$(dirname -- "$target/${overlays[$source]}")"
  cp -- "$root/$source" "$target/${overlays[$source]}"
done
