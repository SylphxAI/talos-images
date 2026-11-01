#!/usr/bin/env bash
# Source checks only. No image build, privileged container, mount or node query.
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
[[ -z $(gofmt -l "$root"/*.go "$root"/talos/*.go) ]] || { echo 'unformatted EFI source' >&2; exit 1; }
(cd "$root" && go test -race -count=1 ./...)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
curl -fsSL --max-time 120 https://codeload.github.com/siderolabs/talos/tar.gz/refs/tags/v1.14.1 -o "$temporary/talos.tar.gz"
printf '%s  %s\n' 49710f8a98f9c98f88453cc2d5ebbfe5c128183785684a9defbc60ad5c76dd55 "$temporary/talos.tar.gz" | sha256sum -c -
tar -xzf "$temporary/talos.tar.gz" -C "$temporary"
source_root="$temporary/talos-1.14.1"
python3 "$root/test_install.py" "$source_root" -v
# Existing Upgrade/Revert and their boot selection code are not patched.
(cd "$source_root" && sha256sum internal/app/machined/pkg/runtime/v1alpha1/bootloader/sdboot/*.go) > "$temporary/boot-before.sha256"
bash "$root/install.sh" "$source_root"
(cd "$source_root" && sha256sum -c "$temporary/boot-before.sha256")
(cd "$source_root" && go test -p 4 ./pkg/machinery/bootinventory ./pkg/machinery/api/machine)
(cd "$source_root" && go test -p 4 ./internal/app/machined/internal/server/v1alpha1 -run TestEFIInventory -count=1)
(cd "$source_root" && go test -p 4 ./internal/app/machined/pkg/runtime/v1alpha1/bootloader/sdboot -count=1)
(cd "$source_root" && go test -p 4 ./cmd/talosctl/cmd/talos -run '^$')
