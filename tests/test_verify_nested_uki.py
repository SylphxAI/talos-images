from pathlib import Path
import runpy
import unittest

verify_cmdline = runpy.run_path(str(Path(__file__).resolve().parents[1] / "scripts/verify-nested-uki.py"))["verify_cmdline"]


class NestedUKICmdlineTest(unittest.TestCase):
    def test_single_exact_parameter(self):
        self.assertEqual(verify_cmdline(b"quiet kvm_amd.nested=1\0"), "quiet kvm_amd.nested=1")

    def test_missing_duplicate_conflicting_or_malformed_parameter(self):
        for suffix in (b"", b"kvm_amd.nested=0", b"foo=kvm_amd.nested=1", b"kvm_amd.nested=11",
                       b"kvm_amd.nested=1 kvm_amd.nested=1", b"kvm_amd.nested=1 kvm-amd.nested=0",
                       b"kvm_amd.nested=1\0 kvm_amd.nested=0"):
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                verify_cmdline(b"quiet " + suffix)


class StockNestedInitrdTest(unittest.TestCase):
    @staticmethod
    def entry(name, content):
        fields = [0] * 13
        fields[6], fields[11] = len(content), len(name) + 1
        header = b"070701" + b"".join(f"{n:08x}".encode() for n in fields)
        name_record = header + name.encode() + b"\0"
        return name_record + b"\0" * (-len(name_record) % 4) + content + b"\0" * (-len(content) % 4)

    def test_checks_exact_extension_set_and_core_rootfs(self):
        import hashlib
        import io
        import json
        import sys
        from types import SimpleNamespace
        from unittest.mock import patch

        verifier = runpy.run_path(str(Path(__file__).resolve().parents[1] / "scripts/verify-stock-nested-uki.py"))
        extract = verifier["extract_kata"]
        expected = verifier["FR1_EXTENSIONS"]
        layers = [{"metadata": {"name": name, "version": version}, "image": f"{i}.sqsh"}
                  for i, (name, version) in enumerate(expected.items())]
        # JSON is a YAML subset; stub its loader so the unit test needs no PyYAML.
        def initrd(root, manifest, extra=b""):
            return (self.entry("rootfs.sqsh", root) + self.entry("extensions.yaml", json.dumps({"layers": manifest}).encode()) +
                    b"".join(self.entry(layer["image"], layer["metadata"]["name"].encode()) for layer in manifest) + extra)

        with patch.dict(sys.modules, yaml=SimpleNamespace(safe_load=json.loads)), patch.dict(
            extract.__globals__, FR1_ROOTFS_SHA256=hashlib.sha256(b"stock rootfs").hexdigest()
        ):
            output = io.BytesIO()
            self.assertEqual(extract(initrd(b"stock rootfs", layers), output, expected, True), expected)
            self.assertEqual(output.getvalue(), b"kata-containers")
            for payload in (initrd(b"modified rootfs", layers), initrd(b"stock rootfs", layers[:-1]),
                            initrd(b"stock rootfs", layers, self.entry("3.sqsh", b"unknown extension"))):
                with self.subTest(payload=payload[:20]), self.assertRaises(ValueError):
                    extract(payload, io.BytesIO(), expected, True)
