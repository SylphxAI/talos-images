"""Check guest kernel inputs, the vmlinux verification gate, and OCI wiring."""

import hashlib
import pathlib
import re
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
BUILDER = ROOT / "guest-kernel" / "build-kernel.sh"
SCRIPT = BUILDER.read_text()
FRAGMENT = (ROOT / "guest-kernel" / "qos.conf").read_text()
NESTED_FRAGMENT = (ROOT / "guest-kernel" / "nested-kvm.conf").read_text()
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
NESTED = {"CONFIG_VIRTUALIZATION", "CONFIG_KVM", "CONFIG_KVM_IOAPIC", "CONFIG_KVM_AMD",
          "CONFIG_KVM_INTEL", "CONFIG_VHOST_MENU", "CONFIG_VHOST_NET"}
DEPENDENCIES = {"CONFIG_KVM_X86", "CONFIG_VHOST", "CONFIG_TUN", "CONFIG_HIGH_RES_TIMERS",
                "CONFIG_PREEMPT_NOTIFIERS"}
INITCALLS = (
    "sch_prio__0_433_prio_module_init6", "sch_htb__1_2165_htb_module_init6",
    "sch_netem__2_1382_netem_module_init6", "cls_flower__3_3869_cls_fl_init6",
    "kvm__4_14451_kvm_x86_init6", "kvm_amd__5_5623_svm_init6",
    "kvm_intel__6_1073_vt_init6", "vhost_net__7_1882_vhost_net_init6",
)
IRQCHIP_PIT_SYMBOLS = ("kvm_pic_init", "kvm_ioapic_init", "kvm_create_pit")


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

    def test_nested_fragment_only_requests_the_required_built_ins(self):
        settings = [line for line in NESTED_FRAGMENT.splitlines() if line and not line.startswith("#")]
        self.assertCountEqual(settings, [f"{option}=y" for option in NESTED])
        merge = re.search(r"merge_config.sh[^\n]*\\\n[^\n]*", SCRIPT)
        self.assertIsNotNone(merge)
        self.assertIn('"$root/guest-kernel/qos.conf" "$root/guest-kernel/nested-kvm.conf"', merge.group())

    def test_verify_requires_config_and_built_in_registration(self):
        with tempfile.TemporaryDirectory() as scratch:
            path = pathlib.Path(scratch) / "src" / f"kata-linux-{pin('KERNEL_VERSION')}-{pin('CONFIG_VERSION')}"
            path.mkdir(parents=True)
            config = path / ".config"
            good_config = "# CONFIG_MODULES is not set\n" + "".join(
                f"{option}=y\n" for option in sorted(NEEDED.keys() | ALSO.keys() | NESTED | DEPENDENCIES)
            )
            config.write_text(good_config)

            def write_elf(initcalls=INITCALLS, text_symbols=IRQCHIP_PIT_SYMBOLS, data_symbols=()):
                asm = ".text\n" + "".join(f"__initcall__kmod_{name}: .byte 0\n" for name in initcalls)
                asm += "".join(f"{name}: .byte 0\n" for name in text_symbols)
                asm += ".data\n" + "".join(f"{name}: .byte 0\n" for name in data_symbols)
                subprocess.run(["as", "-o", path / "vmlinux"], input=asm, text=True, check=True)

            def verify():
                return subprocess.run(["bash", BUILDER, "verify"], text=True, capture_output=True,
                                      env={"WORK_DIR": scratch})

            self.assertIn("no vmlinux", verify().stderr)
            write_elf()
            result = verify()
            self.assertEqual(result.returncode, 0, result.stderr)
            for option, bad in (("CONFIG_KVM_INTEL", "CONFIG_KVM_INTEL=m"),
                                ("CONFIG_KVM_IOAPIC", "# CONFIG_KVM_IOAPIC is not set")):
                with self.subTest(option=option):
                    config.write_text(good_config.replace(f"{option}=y", bad))
                    result = verify()
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"not built in: {option}", result.stderr)
            config.write_text(good_config)
            for name in INITCALLS:
                with self.subTest(initcall=name):
                    # Keep the correct module prefix but an unrelated function.
                    write_elf([x if x != name else x + "_wrong" for x in INITCALLS])
                    result = verify()
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"vmlinux does not register {name.split('__')[0]}", result.stderr)
            for name in IRQCHIP_PIT_SYMBOLS:
                with self.subTest(symbol=name):
                    # Near matches and exact names outside .text are not implementations.
                    write_elf(text_symbols=[x if x != name else x + "_wrong" for x in IRQCHIP_PIT_SYMBOLS],
                              data_symbols=[name])
                    result = verify()
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"vmlinux does not define {name}", result.stderr)

    def test_published_kata_extension_uses_the_verified_kernel(self):
        dockerfile = (ROOT / "host-memory" / "Dockerfile").read_text()
        workflow = (ROOT / ".github" / "workflows" / "build.yml").read_text()
        target_path = "rootfs/usr/local/share/kata-containers/vmlinux.container"
        self.assertIn("bash guest-kernel/build-kernel.sh verify", dockerfile)
        self.assertIn(f"COPY --from=guest-kernel --chmod=0644 /vmlinux.container /{target_path}", dockerfile)
        self.assertIn("push:\n    branches: [main]\n", workflow)
        self.assertIn(f"tar -xOf - {target_path}", workflow)
        self.assertIn('bash guest-kernel/build-kernel.sh symbols "$kernel"', workflow)


if __name__ == "__main__":
    unittest.main()
