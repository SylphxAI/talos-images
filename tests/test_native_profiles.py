"""Exercise the profile selected by the real build and source-check entrypoints."""
import os
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class NativeProfilesTest(unittest.TestCase):
    def profile(self, version):
        return subprocess.run(
            ["bash", "-ec", 'source host-memory/profile.sh; printf "%s\\n" "$TALOS_VERSION" "$CONTAINERD_VERSION" "$CONTAINERD_BASE" "$KATA_BASE" "$TALOS_SHA256" "$CONTAINERD_SHA256"'],
            cwd=ROOT, env={**os.environ, "TALOS_VERSION": version},
            capture_output=True, text=True,
        )

    def test_reviewed_pairs_remain_separate(self):
        de = self.profile("v1.14.1")
        fr = self.profile("v1.14.2")
        self.assertEqual(de.returncode, 0, de.stderr)
        self.assertEqual(fr.returncode, 0, fr.stderr)
        a, b = de.stdout.splitlines(), fr.stdout.splitlines()
        self.assertEqual(a[:2], ["v1.14.1", "2.3.5"])
        self.assertEqual(b[:2], ["v1.14.2", "2.3.6"])
        self.assertNotEqual(a[2:4], b[2:4])
        self.assertEqual(b[4:], [
            "00e51df1940b836fdbc148d8c263823669e20aa5f6424b3be15127c6cc89acc9",
            "b4b407c64e11a22549b14c47857ac0e02e9f47f8cfc28924e9aa04a0adcaa883",
        ])
        for values in (a, b):
            for image in values[2:4]:
                self.assertRegex(image, r"^ghcr.io/siderolabs/[a-z-]+@sha256:[a-f0-9]{64}$")

    def test_unreviewed_abi_fails_closed(self):
        for version in ("v1.14.3", "v1.13.7", "latest"):
            result = self.profile(version)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")
            self.assertIn("reviewed", result.stderr)

    def test_build_entrypoint_passes_the_selected_pair_to_each_target(self):
        revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        with tempfile.TemporaryDirectory() as directory:
            scratch = pathlib.Path(directory)
            docker = scratch / "docker"
            docker.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE"\nexit 42\n')
            docker.chmod(0o755)
            for kind, target in (("kata", "kata-extension"), ("imager", "containerd-package"), ("kubelet", "kubelet-image")):
                capture = scratch / "args"
                result = subprocess.run(["bash", "host-memory/build-oci.sh", kind], cwd=ROOT,
                    env={**os.environ, "TALOS_VERSION": "v1.14.2", "SOURCE_SHA": revision,
                         "OCI_DIR": str(scratch / "oci"), "CAPTURE": str(capture),
                         "PATH": str(scratch) + os.pathsep + os.environ["PATH"]},
                    capture_output=True, text=True)
                self.assertEqual(result.returncode, 42, result.stderr)
                args = capture.read_text().splitlines()
                self.assertEqual(args[args.index("--target") + 1], target)
                self.assertIn("CONTAINERD_VERSION=2.3.6", args)
                self.assertIn("KATA_BASE=ghcr.io/siderolabs/kata-containers@sha256:803def92a6dbbef6746afdb68271b4c079ab10e0faec5c8be79dd2047db6b168", args)


if __name__ == "__main__":
    unittest.main()
