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
