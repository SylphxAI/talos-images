#!/usr/bin/env python3
"""Validate the non-secret native-runtime stack before any image publication."""

import argparse
import json
import os
import re
import sys

DIGEST_IMAGE = re.compile(r"[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?/[a-z0-9]+(?:[._/-][a-z0-9]+)*@sha256:[0-9a-f]{64}")


def validate(value, talos_version):
    expected = {"schema_version", "talos_version", "source_revision", "imager", "kata_extension", "kubelet", "kubelet_version"}
    if not isinstance(value, dict) or not expected.issubset(value) or set(value) - expected - {"installer"}:
        raise ValueError("native runtime release requires the complete imager, Kata and kubelet stack with source and ABI identities")
    if type(value["schema_version"]) is not int or value["schema_version"] != 1:
        raise ValueError("unsupported native runtime release schema")
    if value["talos_version"] != talos_version or not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", talos_version):
        raise ValueError("native runtime release must match the selected Talos version")
    revision = value["source_revision"]
    if not isinstance(revision, str) or not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("native runtime source_revision must be a full lowercase commit SHA")
    for field in ("imager", "kata_extension"):
        ref = value[field]
        if not isinstance(ref, str) or not DIGEST_IMAGE.fullmatch(ref):
            raise ValueError(f"native runtime {field} requires a registry-qualified digest-only image reference")
    if value["kubelet_version"] != "v1.36.4":
        raise ValueError("native runtime requires the supported kubelet v1.36.4 ABI")
    kubelet = value["kubelet"]
    if not isinstance(kubelet, str) or not re.fullmatch(r"[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?/[a-z0-9]+(?:[._/-][a-z0-9]+)*:v1\.36\.4(?:-[a-z0-9][a-z0-9_.-]*)?@sha256:[0-9a-f]{64}", kubelet):
        raise ValueError("native runtime kubelet requires a matching version tag plus immutable digest")
    if len({value[field].rsplit("@", 1)[1] for field in ("imager", "kata_extension", "kubelet")}) != 3:
        raise ValueError("native runtime components must be distinct artifacts")
    if "installer" in value:
        installer = value["installer"]
        if not isinstance(installer, str) or not DIGEST_IMAGE.fullmatch(installer):
            raise ValueError("native runtime installer requires an immutable digest")
        if installer.rsplit("@", 1)[1] in {value[field].rsplit("@", 1)[1] for field in ("imager", "kata_extension", "kubelet")}:
            raise ValueError("native runtime installer must be a distinct artifact")
    return {**value, "artifact_tag": f"{talos_version}-host-memory-{revision}"}

def finish(value, talos_version, installer):
    result = validate(value, talos_version)
    if not isinstance(installer, str) or not DIGEST_IMAGE.fullmatch(installer):
        raise ValueError("native runtime installer requires a registry-qualified digest-only reference")
    if installer.rsplit("@", 1)[1] in {result[field].rsplit("@", 1)[1] for field in ("imager", "kata_extension", "kubelet")}:
        raise ValueError("native runtime installer must be a distinct artifact")
    del result["artifact_tag"]
    return {**result, "installer": installer}



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--installer", help="Finalize the rollout manifest with the published immutable installer")
    args = parser.parse_args()
    try:
        value = json.loads(os.environ["NATIVE_RUNTIME_RELEASE"])
        result = finish(value, os.environ["TALOS_VERSION"], args.installer) if args.installer else validate(value, os.environ["TALOS_VERSION"])
        print(json.dumps(result, sort_keys=True, separators=(",", ":")))
    except (KeyError, ValueError, TypeError) as error:
        print(f"native runtime release: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
