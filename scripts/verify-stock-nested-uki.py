#!/usr/bin/env python3
"""Check the stock nested boot contract and read back the Kata ELF from both UKIs."""

import hashlib
import mmap
from pathlib import Path
import runpy
import struct
import subprocess
import sys
import tempfile

# Use the existing unsigned PE and command-line checks, not its stargz policy.
uki_checks = runpy.run_path(str(Path(__file__).with_name("verify-nested-uki.py")))
require = uki_checks["require"]


def initrd_entries(data):
    """Yield newc records across Talos's concatenated initramfs archives."""
    cursor = 0
    while cursor < len(data):
        if data[cursor] == 0:
            cursor += 1
            continue
        require(data[cursor:cursor + 6] == b"070701", "invalid initrd cpio record")
        size = int(data[cursor + 54:cursor + 62], 16)
        namesize = int(data[cursor + 94:cursor + 102], 16)
        name_end = cursor + 110 + namesize
        name = data[cursor + 110:name_end]
        start = (name_end + 3) & ~3
        cursor = (start + size + 3) & ~3
        require(cursor <= len(data) and name.endswith(b"\0"), "truncated initrd cpio record")
        yield name[:-1], start, size


STOCK_EXTENSIONS = {"kata-containers": "3.32.0"}
FR1_EXTENSIONS = {**STOCK_EXTENSIONS, "stargz-snapshotter": "v0.18.2", "gvisor": "20260831.0"}
# Byte-for-byte rootfs read back from the FR-1 Factory installer (amd64).
FR1_ROOTFS_SHA256 = "905e6953d94f85260bd5fd04c1e5e2643051a05d1227f27844001781d9bb9259"


def extract_kata(data, destination, expected, verify_stock_rootfs=False):
    import yaml  # python3-yaml on the build runner

    manifests = []
    layers = {}
    rootfs = []
    for name, start, size in initrd_entries(data):
        if name == b"extensions.yaml":
            manifests.append(yaml.safe_load(data[start:start + size]))
        elif name == b"rootfs.sqsh":
            rootfs.append((start, size))
        elif name.endswith(b".sqsh") and name[:-5].isdigit():
            require(name not in layers, "duplicate initrd squashfs layer")
            layers[name] = start, size
    require(len(manifests) == 1 and isinstance(manifests[0], dict), "expected one extension manifest")
    configured = manifests[0].get("layers")
    require(isinstance(configured, list) and len(configured) == len(expected),
            "wrong number of system extensions")
    versions = {item["metadata"]["name"]: item["metadata"]["version"] for item in configured}
    require(versions == expected, f"unexpected system extensions: {versions}")
    images = {item["image"].encode() for item in configured}
    require(len(images) == len(expected) and images == set(layers), "extension squashfs mismatch")
    kata = next(item for item in configured if item["metadata"]["name"] == "kata-containers")
    start, size = layers[kata["image"].encode()]
    with memoryview(data) as view:
        if verify_stock_rootfs:
            require(len(rootfs) == 1, "expected exactly one core rootfs squashfs")
            root_start, root_size = rootfs[0]
            digest = hashlib.sha256(view[root_start:root_start + root_size]).hexdigest()
            require(digest == FR1_ROOTFS_SHA256, f"FR-1 stock rootfs SHA-256 mismatch: {digest}")
            print(f"FR-1 Factory stock rootfs SHA-256 (includes stock containerd and runc shim): {digest}")
        destination.write(view[start:start + size])
    return versions


def verify(label, path, expected_hash, version):
    fr1 = version == "v1.14.2"
    with tempfile.TemporaryDirectory() as scratch:
        tmp = Path(scratch)
        compressed = tmp / "initrd.zst"
        with open(path, "rb") as file, mmap.mmap(file.fileno(), 0, access=mmap.ACCESS_READ) as uki:
            table = uki_checks["sections"](uki)
            for name in (b".linux", b".initrd", b".osrel"):
                require(len(table.get(name, [])) == 1, f"missing or duplicate {name!r}")
            offset, size = table[b".osrel"][0]
            osrel = uki[offset:offset + size].rstrip(b"\0").decode()
            require(f"VERSION_ID={version}" in osrel.splitlines() or
                    f'VERSION_ID="{version}"' in osrel.splitlines(), f"expected Talos {version} in .osrel")
            cmdlines = table.get(b".cmdline", [])
            require(len(cmdlines) == 2, "expected default and reset-maintenance .cmdline sections")
            for variant, (offset, size) in zip(("default", "reset-maintenance"), cmdlines):
                cmdline = uki_checks["verify_cmdline"](uki[offset:offset + size])
                print(f"{label} {variant} UKI .cmdline (kvm_amd.nested=1 count=1): {cmdline}")
            offset, size = table[b".initrd"][0]
            with compressed.open("wb") as output, memoryview(uki) as view:
                output.write(view[offset:offset + size])

        squashfs = tmp / "kata.sqsh"
        with tempfile.TemporaryFile() as initrd:
            subprocess.run(["zstd", "-dc", compressed], stdout=initrd, check=True)
            with mmap.mmap(initrd.fileno(), 0, access=mmap.ACCESS_READ) as data, squashfs.open("wb") as out:
                versions = extract_kata(data, out, FR1_EXTENSIONS if fr1 else STOCK_EXTENSIONS, fr1)
        with tempfile.TemporaryFile() as elf:
            subprocess.run(["unsquashfs", "-cat", squashfs, "usr/local/share/kata-containers/vmlinux.container"],
                           stdout=elf, check=True)
            elf.seek(0)
            digest = hashlib.file_digest(elf, "sha256").hexdigest()
        require(digest == expected_hash, f"embedded guest ELF SHA-256 {digest} differs from published Kata")
        print(f"{label}: unsigned amd64 Talos {version} UKI; extensions {versions}; guest ELF SHA-256 {digest}")


if __name__ == "__main__":
    if len(sys.argv) != 5 or sys.argv[4] not in ("v1.14.1", "v1.14.2"):
        sys.exit("usage: verify-stock-nested-uki.py METAL_EFI INSTALLER_EFI GUEST_ELF_SHA256 TALOS_VERSION")
    for label, path in zip(("metal", "installer"), sys.argv[1:3]):
        try:
            verify(label, path, sys.argv[3], sys.argv[4])
        except (ValueError, OSError, KeyError, TypeError, struct.error, subprocess.CalledProcessError) as error:
            sys.exit(f"{label} stock nested UKI verification failed: {error}")
