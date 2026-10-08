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
    def resolve(self, stargz="false", override="", missing_stargz=False):
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


if __name__ == "__main__":
    unittest.main()
