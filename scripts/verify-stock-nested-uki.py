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


def extract_kata(data, destination):
    import yaml  # python3-yaml on the build runner

    manifests = []
    layers = {}
    for name, start, size in initrd_entries(data):
        if name == b"extensions.yaml":
            manifests.append(yaml.safe_load(data[start:start + size]))
        elif name.endswith(b".sqsh") and name[:-5].isdigit():
            require(name not in layers, "duplicate initrd squashfs layer")
            layers[name] = start, size
    require(len(manifests) == 1 and isinstance(manifests[0], dict), "expected one extension manifest")
    configured = manifests[0].get("layers")
    require(isinstance(configured, list) and len(configured) == 1, "expected exactly one system extension")
    layer = configured[0]
    require(layer["metadata"]["name"] == "kata-containers" and layer["metadata"]["version"] == "3.32.0",
            "expected Kata 3.32.0 system extension")
    # Talos v1.14.1 names squashfs files after the zero-based extension index.
    require(list(layers) == [b"0.sqsh"] and layer["image"] == "0.sqsh", "expected exactly one Kata squashfs")
    start, size = layers[b"0.sqsh"]
    with memoryview(data) as view:
        destination.write(view[start:start + size])


def verify(label, path, expected_hash):
    with tempfile.TemporaryDirectory() as scratch:
        tmp = Path(scratch)
        compressed = tmp / "initrd.zst"
        with open(path, "rb") as file, mmap.mmap(file.fileno(), 0, access=mmap.ACCESS_READ) as uki:
            table = uki_checks["sections"](uki)
            for name in (b".linux", b".initrd", b".osrel"):
                require(len(table.get(name, [])) == 1, f"missing or duplicate {name!r}")
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
                extract_kata(data, out)
        with tempfile.TemporaryFile() as elf:
            subprocess.run(["unsquashfs", "-cat", squashfs, "usr/local/share/kata-containers/vmlinux.container"],
                           stdout=elf, check=True)
            elf.seek(0)
            digest = hashlib.file_digest(elf, "sha256").hexdigest()
        require(digest == expected_hash, f"embedded guest ELF SHA-256 {digest} differs from published Kata")
        print(f"{label}: unsigned amd64 UKI; only Kata 3.32.0; guest ELF SHA-256 {digest}")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit("usage: verify-stock-nested-uki.py METAL_EFI INSTALLER_EFI GUEST_ELF_SHA256")
    for label, path in zip(("metal", "installer"), sys.argv[1:3]):
        try:
            verify(label, path, sys.argv[3])
        except (ValueError, OSError, KeyError, TypeError, struct.error, subprocess.CalledProcessError) as error:
            sys.exit(f"{label} stock nested UKI verification failed: {error}")
