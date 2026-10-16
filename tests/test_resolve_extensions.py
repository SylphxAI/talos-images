import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
KATA = "ghcr.io/siderolabs/kata-containers:release@sha256:kata"
STARGZ = "ghcr.io/siderolabs/stargz-snapshotter:release@sha256:stargz"


class ResolveExtensionsTest(unittest.TestCase):
    def resolve(self, stargz="false", override="", missing_stargz=False, native=None):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            catalog = [{"name": "siderolabs/kata-containers", "ref": KATA.split("@")[0], "digest": KATA.split("@")[1]}]
            if not missing_stargz:
                catalog.append({"name": "siderolabs/stargz-snapshotter", "ref": STARGZ.split("@")[0], "digest": STARGZ.split("@")[1]})
            (path / "catalog.json").write_text(json.dumps(catalog))
            (path / "curl").write_text('#!/bin/bash\nprintf "%s\\n" "$*" >> "$CALLS"\n/bin/cat "$CATALOG_FIXTURE"\n')
            (path / "curl").chmod(0o755)
            env = dict(os.environ, PATH=f"{path}:{os.environ['PATH']}",
                       TALOS_VERSION="v1.14.1", STARGZ_ENABLED=stargz,
                       KATA_EXTENSION_IMAGE=override, GITHUB_OUTPUT=str(path / "output"),
                       CATALOG_FIXTURE=str(path / "catalog.json"), CALLS=str(path / "calls"))
            env["NATIVE_RUNTIME_RELEASE"] = json.dumps(native) if native is not None else ""
            result = subprocess.run(["bash", "scripts/resolve-extensions.sh"], cwd=ROOT,
                                    env=env, text=True, capture_output=True)
            output = dict(line.split("=", 1) for line in (path / "output").read_text().splitlines()) if (path / "output").exists() else {}
            calls = (path / "calls").read_text() if (path / "calls").exists() else ""
            return result, output, calls

    def test_default_keeps_kata_only_names(self):
        result, output, calls = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["ref"], KATA)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", KATA])
        self.assertEqual(output["artifact_tag"], "v1.14.1")
        self.assertEqual(output["make_latest"], "true")
        self.assertEqual(output["stargz_ref"], "")
        self.assertIn("/version/v1.14.1/extensions/official", calls)

    def test_stargz_resolves_digest_and_distinct_names(self):
        result, output, _ = self.resolve("true")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", KATA, "--system-extension-image", STARGZ])
        self.assertEqual(output["stargz_ref"], STARGZ)
        self.assertEqual(output["artifact_tag"], "v1.14.1-stargz")
        self.assertEqual(output["make_latest"], "false")

    def test_override_still_resolves_stargz_from_release_catalog(self):
        result, output, calls = self.resolve("true", "custom/kata:tag@sha256:override")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["ref"], "custom/kata:tag@sha256:override")
        self.assertEqual(output["stargz_ref"], STARGZ)
        self.assertIn("/version/v1.14.1/extensions/official", calls)

    def test_kata_override_needs_no_catalog_in_default_mode(self):
        result, output, calls = self.resolve(override="custom/kata:tag@sha256:override")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["ref"], "custom/kata:tag@sha256:override")
        self.assertEqual(calls, "")

    def test_missing_stargz_fails_before_publication(self):
        result, _, _ = self.resolve("true", missing_stargz=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no siderolabs/stargz-snapshotter", result.stderr)

    def native_release(self):
        return {"schema_version": 1, "talos_version": "v1.14.1",
                "source_revision": "a" * 40,
                "imager": "ghcr.io/sylphxai/native-imager@sha256:" + "b" * 64,
                "kata_extension": "ghcr.io/sylphxai/native-kata@sha256:" + "c" * 64,
                "kubelet_version": "v1.36.4",
                "kubelet": "ghcr.io/sylphxai/native-kubelet:v1.36.4@sha256:" + "d" * 64}

    def test_paired_native_artifacts_are_distinct_from_default(self):
        release = self.native_release()
        result, output, calls = self.resolve(native=release)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["imager_ref"], release["imager"])
        self.assertEqual(output["flags"].split(), ["--system-extension-image", release["kata_extension"]])
        self.assertEqual(output["artifact_tag"], "v1.14.1-host-memory-" + release["source_revision"])
        self.assertEqual(output["make_latest"], "false")
        self.assertEqual(calls, "")

    def test_native_stargz_retains_paired_identity_and_catalog_version(self):
        release = self.native_release()
        result, output, calls = self.resolve(stargz="true", native=release)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", release["kata_extension"], "--system-extension-image", STARGZ])
        self.assertEqual(output["artifact_tag"], "v1.14.1-host-memory-" + release["source_revision"] + "-stargz")
        self.assertIn("/version/v1.14.1/extensions/official", calls)

    def test_refuses_mixed_kata_override_before_publication(self):
        result, output, _ = self.resolve(override="custom/kata:other", native=self.native_release())
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, {})
        self.assertIn("cannot be mixed", result.stderr)

    def test_refuses_incomplete_or_mutable_native_release(self):
        invalid = []
        for field in self.native_release():
            value = self.native_release()
            del value[field]
            invalid.append(value)
        for field, entry in [("schema_version", True), ("talos_version", "v1.14.2"),
                             ("source_revision", "short"), ("imager", "ghcr.io/sylphxai/imager:latest"),
                             ("kata_extension", "ghcr.io/sylphxai/kata@sha256:bad"),
                             ("kubelet_version", "v1.37.1"),
                             ("kubelet", "ghcr.io/sylphxai/kubelet:v1.36.4"),
                             ("kubelet", "ghcr.io/sylphxai/kubelet:v1.35.8@sha256:" + "d" * 64),
                             ("imager", "ghcr.io/sylphxai/imager@sha256:" + "a" * 64 + ";echo unsafe")]:
            value = self.native_release()
            value[field] = entry
            invalid.append(value)
        value = self.native_release()
        value["imager"] = value["kata_extension"]
        invalid.append(value)
        for release in invalid:
            with self.subTest(release=release):
                result, output, _ = self.resolve(native=release)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, {})
                self.assertIn("native runtime release", result.stderr)


    def test_final_installer_cannot_be_mutable_or_alias_a_runtime_component(self):
        for installer in ["ghcr.io/sylphxai/installer:latest",
                          "ghcr.io/sylphxai/other-repository@sha256:" + "b" * 64]:
            with self.subTest(installer=installer):
                env = dict(os.environ, TALOS_VERSION="v1.14.1",
                           NATIVE_RUNTIME_RELEASE=json.dumps(self.native_release()))
                result = subprocess.run(["python3", "scripts/runtime-release.py", "--installer", installer],
                                        cwd=ROOT, env=env, text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertIn("installer", result.stderr)


if __name__ == "__main__":
    unittest.main()

