"""The guest kernel build is pinned, and its fragment is exactly what the
cp-etcd-qos baseline needs. The compile itself runs on a build lease; this
checks the inputs that decide what that compile produces."""

import hashlib
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "guest-kernel" / "build-kernel.sh").read_text()
FRAGMENT = (ROOT / "guest-kernel" / "qos.conf").read_text()
VENDOR = ROOT / "guest-kernel" / "vendor"

# What infra/addons/cp-etcd-qos/manifests/qos.sh asks tc for.
NEEDED = {
    "CONFIG_NET_SCH_PRIO": "prio",
    "CONFIG_NET_SCH_HTB": "htb",
    "CONFIG_NET_SCH_NETEM": "netem",
    "CONFIG_NET_CLS_FLOWER": "flower",
}
# cls_flower links against the actions framework, and a classid filter
# installs the generic action. Both have to be built in with it.
ALSO = {"CONFIG_NET_CLS_ACT": "actions", "CONFIG_NET_ACT_GACT": "generic action"}


def pin(name):
    match = re.search(rf"^{name}=(\S+)", SCRIPT, re.M)
    if not match:
        raise AssertionError(f"no {name} pin")
    return match.group(1)


class GuestKernelTest(unittest.TestCase):
    def test_fragment_builds_in_exactly_the_qdiscs_the_baseline_uses(self):
        enabled = dict(re.findall(r"^(CONFIG_\w+)=(y)$", FRAGMENT, re.M))
        self.assertEqual(set(enabled), set(NEEDED) | set(ALSO))
        for option in enabled:
            self.assertNotIn(f"# {option} is not set", FRAGMENT)

    def test_every_vendored_archive_matches_its_recorded_sum(self):
        sums = set(re.findall(r"\b([0-9a-f]{64})\b", SCRIPT))
        archives = sorted(p for p in VENDOR.iterdir() if p.is_file())
        self.assertGreaterEqual(len(archives), 5)
        for archive in archives:
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            self.assertIn(digest, sums, archive.name)

    def test_kernel_and_kata_versions_are_pinned(self):
        self.assertEqual(pin("KATA_VERSION"), "3.32.0")
        self.assertEqual(pin("KERNEL_VERSION"), "6.18.35")
        self.assertEqual(pin("CONFIG_VERSION"), "197")
        self.assertEqual(len(pin("KATA_SHA256")), 64)
        self.assertEqual(len(pin("KERNEL_SHA256")), 64)

    def test_symbol_check_names_the_four_qdisc_initcalls(self):
        named = re.search(r"for name in ([a-z_ ]+); do", SCRIPT)
        self.assertIsNotNone(named)
        self.assertEqual(named.group(1).split(), ["sch_prio", "sch_htb", "sch_netem", "cls_flower"])
        self.assertIn("__initcall__kmod_${name}__", SCRIPT)


if __name__ == "__main__":
    unittest.main()
