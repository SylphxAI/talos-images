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

# flex and bison are not in the lease image, and a lease has no root, so apt
# cannot add them. Their sources are vendored and checked, and they build
# into the workspace with the compiler the image already has. m4 is already
# there; bison needs it.
build_tool() {
  local name=$1 archive=$2 sum=$3
  command -v "$name" >/dev/null && return 0
  local prefix=$work/tools dest=$work/toolsrc/$name
  [ -x "$prefix/bin/$name" ] && { export PATH="$prefix/bin:$PATH"; return 0; }
  echo "$sum  $root/guest-kernel/vendor/$archive" | sha256sum -c -
  mkdir -p "$dest" "$prefix"
  tar -xf "$root/guest-kernel/vendor/$archive" -C "$dest" --strip-components=1
  # Build, then install. One invocation makes `install` race the man page
  # ahead of the binary (flex: "no rule to make target ../src/flex").
  local make_target="all"
  # bc's info manual needs makeinfo, which the lease image does not have.
  # The library and the binary are enough. Separate makes: one chained -C
  # ends in dc, whose rule then looks for the library in the wrong place.
  local install_target="install"
  if [ "$name" = bc ]; then
    make_target="-C lib all && make -C bc all && make -C dc all"
    install_target="-C bc install -C ../dc install"
  fi
  # --disable-dependency-tracking: the lease workspace is virtiofs, and
  # automake's .deps rename fails there with "cannot stat .deps/*.Tpo".
  local log=$work/$name-build.log
  if [ "$name" = zlib ]; then
    # zlib's configure is its own, not autoconf, and takes no automake flags.
    ( cd "$dest" && ./configure --prefix="$prefix" >"$log" 2>&1 && make -j "$(nproc)" >>"$log" 2>&1 && make install >>"$log" 2>&1 ) || {
      echo "zlib failed to build:" >&2; tail -20 "$log" >&2; exit 1; }
  else
    ( cd "$dest" && ./configure --prefix="$prefix" --disable-dependency-tracking >/dev/null && eval "make -j $(nproc) $make_target" >"$log" 2>&1 && eval "make $install_target" >>"$log" 2>&1 ) || {
      echo "$name failed to build; the errors:" >&2
      grep -n -iE 'error|no rule|cannot|missing|not found' "$log" | head -30 >&2
      exit 1
    }
  fi
  export PATH="$prefix/bin:$PATH"
  case "$name" in
    zlib|elfutils) [ -f "$prefix/include/zlib.h" ] || [ -f "$prefix/include/libelf.h" ] || { echo "$name installed no headers" >&2; exit 1; } ;;
    *)
      if ! command -v "$name" >/dev/null; then
        local built
        built=$(find "$dest" -type f -name "$name" -executable | head -1)
        if [ -n "$built" ]; then install -m 0755 "$built" "$prefix/bin/$name"; fi
      fi
      command -v "$name" >/dev/null || { echo "built $name but it is not on PATH" >&2; exit 1; } ;;
  esac
}

toolchain() {
  # m4 first: bison refuses to configure without one that supports -P, and
  # the kernel build uses it too.
  build_tool m4 m4_1.4.21.orig.tar.xz f25c6ab51548a73a75558742fb031e0625d6485fe5f9155949d6486a2408ab66
  build_tool flex flex_2.6.4.orig.tar.gz e87aae032bf07c26f85ac0ed3250998c37621d95f8bd748b31f15b33c45ee995
  # The GNU release, not Debian's dfsg repack: the repack drops the generated
  # docs and its gnulib does not compile against the lease's glibc.
  build_tool bison bison-3.8.2.tar.gz 06c9e13bdf7eb24d4ceb6b59205a4f67c2c7e7213119644430fe82fbd14a0abb
  # bc's build runs `ed` to generate libmath.h.
  build_tool ed ed_1.22.5.orig.tar.gz 3e411ca3a799cccd90b185a2ad14c90a9d5d2899620e95277c2d4838a6394722
  build_tool bc bc_1.07.1.orig.tar.gz 62adfca89b0a1c0164c2cdca59ca210c1d44c3ffc46daf9931cf4942664cb02a
  # objtool includes <gelf.h>. The lease has libelf but not its headers, and
  # no root, so the headers come from a vendored elfutils. elfutils needs
  # zlib's headers, which the image also lacks.
  if [ ! -f "$work/tools/include/zlib.h" ]; then
    build_tool zlib zlib-1.3.1.tar.gz 9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23
  fi
  if [ ! -f "$work/tools/include/libelf.h" ]; then
    export CPPFLAGS="-I$work/tools/include ${CPPFLAGS:-}" LDFLAGS="-L$work/tools/lib ${LDFLAGS:-}"
    build_tool elfutils elfutils_0.196.orig.tar.bz2 fd5cc6b77ad6773cac93cb3f415f9318ac3b3455eecf801f6b4a742c4f6c7209
  fi
  command -v gcc >/dev/null || { echo "gcc is missing" >&2; exit 1; }
  export PATH="$work/tools/bin:$PATH"
  export HOSTCFLAGS="-I$work/tools/include ${HOSTCFLAGS:-}"
  export HOSTLDFLAGS="-L$work/tools/lib ${HOSTLDFLAGS:-}"
}

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
  local url=$1 dest=$2 sum=$3 got=0
  if [ -f "$dest" ] && echo "$sum  $dest" | sha256sum -c - >/dev/null 2>&1; then
    return
  fi
  # The host itself first. A lease that was not granted it fails fast, and
  # the build cache mirror is the second try.
  if curl -fsSL --retry 2 --max-time 240 -o "$dest.partial" "$url"; then
    got=1
  else
    rm -f "$dest.partial"
    curl -fsSL --retry 2 --max-time 240 -o "$dest.partial" "$(mirror_url "$url")" && got=1
  fi
  [ "$got" = 1 ] || { echo "could not fetch $url" >&2; exit 1; }
  mv "$dest.partial" "$dest"
  echo "$sum  $dest" | sha256sum -c -
}

setup() {
  need curl; need sha256sum; need tar; need make
  toolchain
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
  local tree
  tree=$(kernel_tree)
  # Kata's setup returns immediately when the tree already exists, which
  # skips the fragment merge. Drop the marker it checks so it regenerates
  # .config.generated. The sources stay; only the generated config is redone.
  rm -rf "$tree/.config.generated"
  ( cd "$src" && bash "$builder" -f -v "$KERNEL_VERSION" -k "$tree" setup )
  [ -f "$tree/.config.generated" ] || { echo "kata setup produced no config" >&2; exit 1; }

  cd "$tree"
  # -r: the last file wins, so our fragment overrides the stock "is not set".
  KCONFIG_CONFIG=.config scripts/kconfig/merge_config.sh -r -n .config.generated "$root/guest-kernel/qos.conf" >"$work/merge.log" 2>&1 || {
    echo "config merge failed:" >&2; tail -20 "$work/merge.log" >&2; exit 1; }
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

symbols() {
  local tree vmlinux missing=0 name
  tree=$(kernel_tree)
  vmlinux=$tree/vmlinux
  [ -f "$vmlinux" ] || { echo "no vmlinux; run build first" >&2; exit 1; }
  for name in prio_qdisc_ops htb_qdisc_ops netem_qdisc_ops cls_flower_ops; do
    if ! nm "$vmlinux" | grep -q " ${name}$"; then
      echo "vmlinux has no $name" >&2
      missing=1
    fi
  done
  [ "$missing" = 0 ]
  echo "vmlinux links prio, htb, netem and flower"
}

build() {
  local tree jobs
  tree=$(kernel_tree)
  [ -f "$tree/.config" ] || { echo "no config; run setup first" >&2; exit 1; }
  verify
  toolchain
  jobs=${JOBS:-$(nproc)}
  # The vendored tools and libelf headers live in the workspace, not the image.
  export PATH="$work/tools/bin:$PATH"
  make -C "$tree" -j "$jobs" \
    "HOSTCFLAGS=-I$work/tools/include" "HOSTLDFLAGS=-L$work/tools/lib" vmlinux
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
  symbols) symbols ;;
  package) package ;;
  *) echo "usage: $0 setup|verify|build|package" >&2; exit 2 ;;
esac
