"""The stock workflow can only build the kernel-only Kata target."""

import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
STOCK_KATA_BASE = "ghcr.io/siderolabs/kata-containers@sha256:cbc2653c8366ae56e69254a7db4b6f71d576b5371467eb8933c2cd053cbc6b21"


class StockNestedBuildTest(unittest.TestCase):
    def test_docker_recipe_copies_only_the_shared_verified_guest_elf(self):
        recipe = (ROOT / "host-memory/Dockerfile").read_text()
        stages = {name: (base, instructions.strip()) for base, name, instructions in
                  re.findall(r"^FROM (\S+) AS (\S+)\n(.*?)(?=^FROM |\Z)", recipe, re.M | re.S)}
        base, kernel_overlay = stages["kata-with-guest-kernel"]
        self.assertEqual(base, "${KATA_BASE}")
        self.assertEqual([line for line in kernel_overlay.splitlines() if line and not line.startswith("#")], [
            "COPY --from=guest-kernel --chmod=0644 /vmlinux.container /rootfs/usr/local/share/kata-containers/vmlinux.container"
        ])
        self.assertIn("&& bash guest-kernel/build-kernel.sh verify", stages["guest-kernel"][1])
        self.assertEqual(stages["kata-extension"][0], "kata-with-guest-kernel")
        self.assertIn("COPY --from=native", stages["kata-extension"][1])

    def test_preflight_rejects_overrides_before_building(self):
        with tempfile.TemporaryDirectory() as directory:
            scratch = Path(directory)
            docker = scratch / "docker"
            docker.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE"\nexit 42\n')
            docker.chmod(0o755)
            capture = scratch / "args"
            env = dict(os.environ, PATH=f"{scratch}:{os.environ['PATH']}", CAPTURE=str(capture),
                       TALOS_VERSION="v1.14.1", GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="2",
                       NATIVE_HOST_MEMORY="false", NATIVE_RUNTIME_RELEASE="", KATA_EXTENSION_IMAGE="",
                       STARGZ_ENABLED="false", GITHUB_OUTPUT=str(scratch / "output"), RUNNER_TEMP=directory)
            for key, value in {"TALOS_VERSION": "v1.14.3", "NATIVE_HOST_MEMORY": "true", "NATIVE_RUNTIME_RELEASE": "{}",
                               "KATA_EXTENSION_IMAGE": STOCK_KATA_BASE, "STARGZ_ENABLED": "true"}.items():
                with self.subTest(key=key):
                    result = subprocess.run(["bash", "scripts/build-stock-nested-kata.sh"], cwd=ROOT,
                                            env={**env, key: value}, text=True, capture_output=True)
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertFalse(capture.exists())
            result = subprocess.run(["bash", "scripts/build-stock-nested-kata.sh"], cwd=ROOT,
                                    env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 42, result.stderr)
            args = capture.read_text().splitlines()
            self.assertEqual(args[args.index("--platform") + 1], "linux/amd64")
            self.assertEqual(args[args.index("--target") + 1], "kata-with-guest-kernel")
            self.assertIn(f"KATA_BASE={STOCK_KATA_BASE}", args)
            self.assertIn("type=image,name=ghcr.io/sylphxai/talos-installer:kata-v1.14.1-stock-uki-nested-kvm-123-2,"
                          "push=true,oci-mediatypes=true", args)


if __name__ == "__main__":
    unittest.main()
