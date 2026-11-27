#!/usr/bin/env bash
# Build the stock extension and prove the exact registry bytes before use.
set -euo pipefail
source scripts/stock-nested-profile.sh
: "${GITHUB_OUTPUT:?}" "${RUNNER_TEMP:?}"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
elf_path=rootfs/usr/local/share/kata-containers/vmlinux.container
epoch=$(git show -s --format=%ct HEAD)
docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
  --file host-memory/Dockerfile --target kata-with-guest-kernel \
  --build-arg "KATA_BASE=$STOCK_NESTED_KATA_BASE" --build-arg "SOURCE_DATE_EPOCH=$epoch" \
  --metadata-file "$work/build.json" \
  --output "type=image,name=$STOCK_NESTED_KATA_TARGET,push=true,oci-mediatypes=true" .
digest=$(crane digest "$STOCK_NESTED_KATA_TARGET")
test "$digest" = "$(jq -er '.["containerimage.digest"]' "$work/build.json")"
kata_ref="$STOCK_NESTED_REPO@$digest"
crane manifest "$STOCK_NESTED_KATA_BASE" > "$work/base.json"
# An index, changed base layers, or extra filesystem layers are not accepted.
layer=$(crane manifest "$kata_ref" | jq -er --slurpfile base "$work/base.json" '
  select([.layers[:-1][].digest] == [$base[0].layers[].digest]) | .layers[-1].digest')
for image in "$STOCK_NESTED_KATA_BASE" "$kata_ref"; do
  # BuildKit adds a default PATH to a base image without one.
  crane config "$image" | jq -S '
    del(.config.Env[]? | select(. == "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"))
    | if .config.Env == [] then del(.config.Env) else . end
    | {architecture, os, config}' > "$work/${image##*@}.config"
done
diff -u "$work/${STOCK_NESTED_KATA_BASE##*@}.config" "$work/${kata_ref##*@}.config"
crane blob "$STOCK_NESTED_REPO@$layer" > "$work/kernel-layer.tar.gz"
# BuildKit may emit directory entries in the COPY layer. Reject every other entry.
entries=$(tar -tf "$work/kernel-layer.tar.gz" | sed 's|^\./||' | grep -vE '^(|rootfs/|rootfs/usr/|rootfs/usr/local/|rootfs/usr/local/share/|rootfs/usr/local/share/kata-containers/)$')
test "$entries" = "$elf_path"
kernel="$work/vmlinux.container"
crane export "$kata_ref" - | tar -xOf - "$elf_path" > "$kernel"
LC_ALL=C readelf -h "$kernel" | tee "$RUNNER_TEMP/stock-nested-guest-elf.txt"
grep -Eq 'Machine: +Advanced Micro Devices X86-64$' "$RUNNER_TEMP/stock-nested-guest-elf.txt"
grep -Eq 'Type: +EXEC ' "$RUNNER_TEMP/stock-nested-guest-elf.txt"
bash guest-kernel/build-kernel.sh symbols "$kernel" | tee -a "$RUNNER_TEMP/stock-nested-guest-elf.txt"
kernel_sha=$(sha256sum "$kernel" | cut -d' ' -f1)
printf 'kata_ref=%s\nguest_elf_sha256=%s\n' "$kata_ref" "$kernel_sha" >> "$GITHUB_OUTPUT"
printf 'Kata extension: %s\nGuest ELF SHA-256: %s\n' "$kata_ref" "$kernel_sha"
