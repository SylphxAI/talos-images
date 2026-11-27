#!/usr/bin/env python3
"""Fail before publication unless both finished UKIs match the nested boot contract."""

import mmap
import re
import struct
import subprocess
import sys


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sections(uki):
    pe, = struct.unpack_from("<I", uki, 0x3C)
    require(uki[:2] == b"MZ" and uki[pe:pe + 4] == b"PE\0\0", "not a PE image")
    machine, count = struct.unpack_from("<HH", uki, pe + 4)
    optional_size, = struct.unpack_from("<H", uki, pe + 20)
    opt = pe + 24
    require(machine == 0x8664 and optional_size >= 152 and
            struct.unpack_from("<H", uki, opt)[0] == 0x20B and
            struct.unpack_from("<H", uki, opt + 68)[0] == 10, "not an amd64 EFI application")
    # PE data-directory entry 4 is the Authenticode certificate table.
    require(struct.unpack_from("<II", uki, opt + 144) == (0, 0), "baseline UKI is unsigned")
    result = {}
    for i in range(count):
        name, size, _, stored, offset = struct.unpack_from("<8sIIII", uki, opt + optional_size + i * 40)
        require(offset == 0 or offset + min(size, stored) <= len(uki), "invalid PE section bounds")
        if name.rstrip(b"\0") in (b".linux", b".initrd", b".osrel", b".cmdline"):
            require(offset and 0 < size <= stored, "empty or truncated UKI section")
        result.setdefault(name.rstrip(b"\0"), []).append((offset, size))
    return result


def verify_cmdline(raw):
    cmdline = raw.rstrip(b"\0").decode("ascii")
    require(all(32 <= ord(ch) < 127 for ch in cmdline), "invalid .cmdline bytes")
    tokens = cmdline.split()
    nested = [token for token in tokens if token.split("=", 1)[0].replace("-", "_") == "kvm_amd.nested"]
    require(nested == ["kvm_amd.nested=1"], "expected exactly one kvm_amd.nested=1 and no conflicting setting")
    return cmdline


def has_stargz(initrd):
    # Talos 1.14.1 emits concatenated zstd frames containing newc archives.
    cursor = 0
    manifests = []
    while cursor < len(initrd):
        if initrd[cursor] == 0:
            cursor += 1
            continue
        require(initrd[cursor:cursor + 6] == b"070701", "invalid initrd cpio record")
        size = int(initrd[cursor + 54:cursor + 62], 16)
        namesize = int(initrd[cursor + 94:cursor + 102], 16)
        name = initrd[cursor + 110:cursor + 110 + namesize]
        start = (cursor + 110 + namesize + 3) & ~3
        cursor = (start + size + 3) & ~3
        require(cursor <= len(initrd) and name.endswith(b"\0"), "truncated initrd cpio record")
        if name == b"extensions.yaml\0":
            manifests.append(initrd[start:start + size])
    return len(manifests) == 1 and re.search(
        rb"(?m)^ +name: stargz-snapshotter\n +version: v0\.18\.2$", manifests[0]
    ) is not None


def verify(label, path):
    with open(path, "rb") as file, mmap.mmap(file.fileno(), 0, access=mmap.ACCESS_READ) as uki:
        table = sections(uki)
        for name in (b".linux", b".initrd", b".osrel"):
            require(len(table.get(name, [])) == 1 and all(table[name][0]), f"missing {name!r}")
        cmdlines = table.get(b".cmdline", [])
        require(len(cmdlines) == 2, "expected default and reset-maintenance .cmdline sections")
        for variant, (offset, size) in zip(("default", "reset-maintenance"), cmdlines):
            require(offset and size, "empty .cmdline")
            cmdline = verify_cmdline(uki[offset:offset + size])
            print(f"{label} {variant} UKI .cmdline (kvm_amd.nested=1 count=1): {cmdline}")
        offset, size = table[b".initrd"][0]
        initrd = subprocess.run(["zstd", "-dc"], input=uki[offset:offset + size], capture_output=True, check=True).stdout
        require(has_stargz(initrd), "expected stargz-snapshotter v0.18.2 in embedded extension manifest")
        print(f"{label}: unsigned amd64 UKI (matches baseline); initrd includes stargz-snapshotter v0.18.2")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: verify-nested-uki.py METAL_EFI INSTALLER_EFI")
    for label, path in zip(("metal", "installer"), sys.argv[1:]):
        try:
            verify(label, path)
        except (ValueError, OSError, struct.error, subprocess.CalledProcessError) as error:
            sys.exit(f"{label} UKI verification failed: {error}")
