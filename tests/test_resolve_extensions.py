import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
KATA = "ghcr.io/siderolabs/kata-containers:release@sha256:" + "a" * 64
STARGZ = "ghcr.io/siderolabs/stargz-snapshotter:release@sha256:" + "b" * 64
GVISOR = "ghcr.io/siderolabs/gvisor:release@sha256:" + "c" * 64


class ResolveExtensionsTest(unittest.TestCase):
    def resolve(self, stargz="false", override="", missing_stargz=False, native=None,
                gvisor="false", gvisor_ref=GVISOR, **settings):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            catalog = [{"name": "siderolabs/kata-containers", "ref": KATA.split("@")[0], "digest": KATA.split("@")[1]}]
            if not missing_stargz:
                catalog.append({"name": "siderolabs/stargz-snapshotter", "ref": STARGZ.split("@")[0], "digest": STARGZ.split("@")[1]})
            if gvisor_ref is not None:
                ref, digest = gvisor_ref.split("@")
                catalog.append({"name": "siderolabs/gvisor", "ref": ref, "digest": digest})
            (path / "catalog.json").write_text(json.dumps(catalog))
            (path / "curl").write_text('#!/bin/bash\nprintf "%s\\n" "$*" >> "$CALLS"\n/bin/cat "$CATALOG_FIXTURE"\n')
            (path / "curl").chmod(0o755)
            env = dict(os.environ, PATH=f"{path}:{os.environ['PATH']}",
                       TALOS_VERSION="v1.14.1", STARGZ_ENABLED=stargz, GVISOR_ENABLED=gvisor,
                       KATA_EXTENSION_IMAGE=override, GITHUB_OUTPUT=str(path / "output"),
                       CATALOG_FIXTURE=str(path / "catalog.json"), CALLS=str(path / "calls"),
                       UKI_NESTED_KVM="false", NATIVE_HOST_MEMORY="false")
            env["NATIVE_RUNTIME_RELEASE"] = json.dumps(native) if native is not None else ""
            env.update(settings)
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
        self.assertEqual(output["gvisor_ref"], "")
        self.assertIn("/version/v1.14.1/extensions/official", calls)

    def test_stargz_resolves_digest_and_distinct_names(self):
        result, output, _ = self.resolve("true")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", KATA, "--system-extension-image", STARGZ])
        self.assertEqual(output["stargz_ref"], STARGZ)
        self.assertEqual(output["artifact_tag"], "v1.14.1-stargz")
        self.assertEqual(output["make_latest"], "false")

    def test_gvisor_alone_resolves_catalog_with_kata_override(self):
        override = "custom/kata:tag@sha256:override"
        result, output, calls = self.resolve(gvisor="true", override=override)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", override, "--system-extension-image", GVISOR])
        self.assertEqual(output["gvisor_ref"], GVISOR)
        self.assertEqual(output["stargz_ref"], "")
        self.assertEqual(output["artifact_tag"], "v1.14.1-gvisor")
        self.assertEqual(output["make_latest"], "false")
        self.assertIn("/version/v1.14.1/extensions/official", calls)

    def test_fr_native_kata_with_both_factory_extensions(self):
        release = {**self.native_release(), "talos_version": "v1.14.2"}
        result, output, calls = self.resolve(stargz="true", gvisor="true", native=release, TALOS_VERSION="v1.14.2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", release["kata_extension"],
                                                   "--system-extension-image", STARGZ, "--system-extension-image", GVISOR])
        self.assertEqual(output["gvisor_ref"], GVISOR)
        self.assertEqual(output["stargz_ref"], STARGZ)
        self.assertEqual(output["imager_ref"], release["imager"])
        self.assertEqual(output["kubelet_ref"], release["kubelet"])
        self.assertEqual(output["artifact_tag"], f"v1.14.2-host-memory-{release['source_revision']}-stargz-gvisor")
        self.assertEqual(output["make_latest"], "false")
        self.assertEqual(calls.splitlines(), ["-fsSL https://factory.talos.dev/version/v1.14.2/extensions/official"])

    def test_missing_or_unpinned_gvisor_fails_before_publication(self):
        for ref in (None, "ghcr.io/siderolabs/gvisor:release@", "ghcr.io/siderolabs/gvisor:release@sha256:short"):
            with self.subTest(ref=ref):
                result, output, _ = self.resolve(gvisor="true", gvisor_ref=ref)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, {})
                self.assertIn("no siderolabs/gvisor with a valid pinned reference", result.stderr)

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

    def stock_nested(self, **changes):
        settings = {"UKI_NESTED_KVM": "true", "GITHUB_RUN_ID": "100", "GITHUB_RUN_ATTEMPT": "2",
                    "STOCK_NESTED_KATA_REF": "ghcr.io/sylphxai/talos-installer@sha256:" + "c" * 64}
        settings.update(changes)
        return self.resolve(**settings)

    def test_nested_uki_is_stock_kata_only_and_run_qualified(self):
        result, output, calls = self.stock_nested()
        self.assertEqual(result.returncode, 0, result.stderr)
        imager = "ghcr.io/siderolabs/imager@sha256:bb74a5c4fc190997cca0930004978203f1e2e2cd637f4db1f26ef3b2eaef8338"
        installer = "ghcr.io/siderolabs/installer-base@sha256:816b11f0bc34b7bf7b34e4fe5b3764c3056d7f66f8098559f8ca750b37d0948d"
        kata = "ghcr.io/sylphxai/talos-installer@sha256:" + "c" * 64
        self.assertEqual(output["imager_ref"], imager)
        self.assertEqual(output["ref"], kata)
        self.assertEqual(output["flags"].split(), ["--system-extension-image", kata, "--base-installer-image", installer,
                                                  "--extra-kernel-arg", "kvm_amd.nested=1"])
        self.assertEqual(output["stargz_ref"], "")
        self.assertEqual(output["kubelet_ref"], "")
        self.assertEqual(output["gvisor_ref"], "")
        self.assertNotIn("native_manifest", output)
        self.assertEqual(output["make_latest"], "false")
        self.assertEqual(output["artifact_tag"], "v1.14.1-stock-uki-nested-kvm-100-2")
        self.assertEqual(calls, "")
        self.assertEqual(self.stock_nested(GITHUB_RUN_ATTEMPT="3")[1]["artifact_tag"],
                         "v1.14.1-stock-uki-nested-kvm-100-3")

    def test_nested_uki_requires_an_internal_digest_and_run_identity(self):
        for changes in ({"STOCK_NESTED_KATA_REF": ""}, {"STOCK_NESTED_KATA_REF": "sylphxai/kata:latest"},
                        {"STOCK_NESTED_KATA_REF": "ghcr.io/other/kata@sha256:" + "a" * 64},
                        {"GITHUB_RUN_ID": ""}, {"GITHUB_RUN_ATTEMPT": "-1"}):
            with self.subTest(changes=changes):
                result, output, calls = self.stock_nested(**changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, {})
                self.assertEqual(calls, "")

    def test_fr1_nested_uki_uses_exact_amd64_factory_set_without_native_components(self):
        result, output, calls = self.stock_nested(TALOS_VERSION="v1.14.2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, "")
        self.assertEqual(output["kubelet_ref"], "")
        self.assertEqual(output["make_latest"], "false")
        self.assertEqual(output["artifact_tag"], "v1.14.2-stock-uki-nested-kvm-fr1-100-2")
        self.assertEqual(output["imager_ref"], "ghcr.io/siderolabs/imager@sha256:a09ccdeeffd8e5b5cce3d4a23599dfd527ff9043242a18c1bff45f469cec3cbe")
        self.assertEqual(output["stargz_ref"], "ghcr.io/siderolabs/stargz-snapshotter@sha256:be40f902374211bc841682e5899f6268b2529c61f7fc56f187b2e0cd8e34342a")
        self.assertEqual(output["gvisor_ref"], "ghcr.io/siderolabs/gvisor@sha256:93fd4332e0ae8c9f4a51fe8590eba16a1ffc282bac836ceaae75e6fbbfddc2d7")
        self.assertEqual(output["flags"].split(), ["--system-extension-image", output["ref"],
                                                  "--system-extension-image", output["stargz_ref"],
                                                  "--system-extension-image", output["gvisor_ref"],
                                                  "--base-installer-image", "ghcr.io/siderolabs/installer-base@sha256:90472c8dac7a0d79742951057c0bdd26e8f0e0027d837c48723666b6b6d50d61",
                                                  "--extra-kernel-arg", "kvm_amd.nested=1"])

    def test_nested_uki_rejects_other_stacks_before_publication(self):
        for field, value in {"TALOS_VERSION": "v1.14.3", "NATIVE_HOST_MEMORY": "true", "STARGZ_ENABLED": "true",
                             "GVISOR_ENABLED": "true", "KATA_EXTENSION_IMAGE": KATA,
                             "NATIVE_RUNTIME_RELEASE": "{}"}.items():
            with self.subTest(field=field):
                result, output, calls = self.stock_nested(**{field: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("the stock nested UKI profile", result.stderr)
                self.assertEqual(output, {})
                self.assertEqual(calls, "")

    def test_v2_native_receipt_reuses_pinned_artifacts(self):
        release = {**self.native_release(), "schema_version": 2}
        result, output, _ = self.resolve(native=release)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output["imager_ref"], release["imager"])
        self.assertEqual(output["kubelet_ref"], release["kubelet"])

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
        for field, entry in [("schema_version", True), ("schema_version", 3), ("talos_version", "v1.14.2"),
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

