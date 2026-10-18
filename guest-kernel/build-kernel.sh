#!/usr/bin/env bash
# Build the Kata guest kernel with the traffic-control options a build lease
# needs, and check the result into the vmlinux it produced.
#
# The recipe is Kata's own (tools/packaging/kernel/build-kernel.sh at the
# release named by KATA_VERSION): the same kernel version, the same fragment
# merge, the same `make oldconfig`, the same `make`. The only addition is
# guest-kernel/qos.conf, merged in last so it wins over the stock fragments.
#
# Everything is pinned. The Kata source archive and the kernel tarball are
# checked against recorded sha256 sums, and the script refuses to run unless
# the kernel version and config version it finds in that source match the
# pins below. A newer Kata release needs a deliberate pin bump, not a silent
# rebuild of a different kernel.
#
#   guest-kernel/build-kernel.sh setup    download, merge config, oldconfig
#   guest-kernel/build-kernel.sh build    compile vmlinux
#   guest-kernel/build-kernel.sh verify   the four options are =y in .config
#   guest-kernel/build-kernel.sh package  vmlinux plus its config, as a tar
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=${WORK_DIR:-$root/guest-kernel/out}
src=$work/src

KATA_VERSION=3.32.0
KATA_SHA256=722a022d873e6742788eca9f6ed74e8b7c4f70ffe4713144bd2e91fc3db620d0
KERNEL_VERSION=6.18.35
KERNEL_SHA256=f78602932219125e211c5f5bfd84edcfd4ec5ce88fc944f8248413f665bef236
CONFIG_VERSION=197

kata_tree() { echo "$src/kata-containers-$KATA_VERSION"; }
kernel_tree() { echo "$src/kata-linux-$KERNEL_VERSION-$CONFIG_VERSION"; }

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }

# A build lease has no route to the public internet. It fetches through the
# build cache's upstream mirror, which keeps the host and path of the original
# URL. Outside a lease the mirror is unset and the URL is used as written.
mirror_url() {
  local url=$1 base
  base=${SYLPHX_UPSTREAM_MIRROR:-}
  if [ -n "$base" ]; then
    printf '%s/%s\n' "${base%/}" "${url#https://}"
  else
    printf '%s\n' "$url"
  fi
}

fetch() {
  local url=$1 dest=$2 sum=$3
  if [ -f "$dest" ] && echo "$sum  $dest" | sha256sum -c - >/dev/null 2>&1; then
    return
  fi
  curl -fsSL --retry 3 --max-time 600 -o "$dest.partial" "$(mirror_url "$url")"
  mv "$dest.partial" "$dest"
  echo "$sum  $dest" | sha256sum -c -
}

setup() {
  need curl; need sha256sum; need tar; need make
  mkdir -p "$src"
  fetch "https://github.com/kata-containers/kata-containers/archive/refs/tags/$KATA_VERSION.tar.gz" \
    "$src/kata-$KATA_VERSION.tar.gz" "$KATA_SHA256"
  if [ ! -d "$(kata_tree)" ]; then
    tar -xzf "$src/kata-$KATA_VERSION.tar.gz" -C "$src"
  fi

  local kata found_kernel found_config
  kata=$(kata_tree)
  found_kernel=$(sed -n 's/^    version: "v\{0,1\}\([0-9.]*\)".*/\1/p' "$kata/versions.yaml" | head -1)
  found_config=$(tr -d '[:space:]' < "$kata/tools/packaging/kernel/kata_config_version")
  [ "$found_kernel" = "$KERNEL_VERSION" ] || { echo "kernel pin $KERNEL_VERSION but source says $found_kernel" >&2; exit 1; }
  [ "$found_config" = "$CONFIG_VERSION" ] || { echo "config pin $CONFIG_VERSION but source says $found_config" >&2; exit 1; }

  local major builder
  major=${KERNEL_VERSION%%.*}
  fetch "https://cdn.kernel.org/pub/linux/kernel/v$major.x/linux-$KERNEL_VERSION.tar.xz" \
    "$src/linux-$KERNEL_VERSION.tar.xz" "$KERNEL_SHA256"

  if [ ! -d "$(kernel_tree)" ]; then
    tar -xJf "$src/linux-$KERNEL_VERSION.tar.xz" -C "$src"
    mv "$src/linux-$KERNEL_VERSION" "$(kernel_tree)"
  fi

  builder=$kata/tools/packaging/kernel/build-kernel.sh
  # Kata's setup merges its own fragments and runs oldconfig. Point it at the
  # tree we just unpacked so it does not download the kernel a second time.
  ( cd "$src" && bash "$builder" -v "$KERNEL_VERSION" -k "$(kernel_tree)" setup )

  # Our fragment wins: merge_config.sh -r lets the last file override.
  local tree
  tree=$(kernel_tree)
  cd "$tree"
  KCONFIG_CONFIG=.config scripts/kconfig/merge_config.sh -r -n .config "$root/guest-kernel/qos.conf" >/dev/null
  make olddefconfig
  verify
}

verify() {
  local tree config missing=0 option
  tree=$(kernel_tree)
  config=$tree/.config
  [ -f "$config" ] || { echo "no $config; run setup first" >&2; exit 1; }
  for option in CONFIG_NET_SCH_PRIO CONFIG_NET_SCH_HTB CONFIG_NET_SCH_NETEM \
                CONFIG_NET_CLS_ACT CONFIG_NET_ACT_GACT CONFIG_NET_CLS_FLOWER; do
    if ! grep -qx "$option=y" "$config"; then
      echo "not built in: $option" >&2
      missing=1
    fi
  done
  [ "$missing" = 0 ]
  echo "guest kernel config has prio, htb, netem and flower built in"
}

build() {
  local tree jobs
  tree=$(kernel_tree)
  [ -f "$tree/.config" ] || { echo "no config; run setup first" >&2; exit 1; }
  verify
  jobs=${JOBS:-$(nproc)}
  make -C "$tree" -j "$jobs" vmlinux
  [ -f "$tree/vmlinux" ]
}

package() {
  local tree out
  tree=$(kernel_tree)
  [ -f "$tree/vmlinux" ] || { echo "no vmlinux; run build first" >&2; exit 1; }
  out=$work/vmlinux-qos.tar
  tar -cf "$out" -C "$tree" vmlinux .config System.map
  echo "$out"
}

case ${1:-} in
  setup) setup ;;
  verify) verify ;;
  build) build ;;
  package) package ;;
  *) echo "usage: $0 setup|verify|build|package" >&2; exit 2 ;;
esac
