# Talos Images

<p align="center">
  <img src="https://mark.sylphx.com/api/v1/mark/hero.svg?type=aurora&theme=grape&text=talos-images&desc=Talos%20Linux%20images%20with%20Kata%20Containers" alt="talos-images" width="100%" />
</p>

[Talos Linux](https://www.talos.dev) images for bare-metal servers, built with
the [Kata Containers](https://katacontainers.io) system extension so pods can
run in lightweight virtual machines.

The managed default builds the reviewed paired native runtime on the supported
Talos version, without changing any node. Stock/advanced builds still resolve
the compatible Kata extension from the [Sidero image factory](https://factory.talos.dev)
or accept the explicit override. [extensions.yaml](extensions.yaml) explains
that path; moving the native runtime to another Talos ABI requires a reviewed port.

## Outputs

| Artifact | Format | Use |
|----------|--------|-----|
| `metal-amd64.raw.zst`, on the [release](https://github.com/SylphxAI/talos-images/releases) named after the Talos version | Raw disk image | First install: write it to the server's disk |
| `ghcr.io/sylphxai/talos-installer:{talos-version}` | OCI image | `talosctl upgrade --image ...` |
| `metal-amd64.raw.zst`, on release `{talos-version}-stargz` | Raw disk image | Opt-in Kata + stargz first install |
| `ghcr.io/sylphxai/talos-installer:{talos-version}-stargz` | OCI image | Opt-in Kata + stargz upgrade |
| `metal-amd64.raw.zst`, on release `{talos-version}-host-memory-{source-revision}` | Raw disk image | Reviewed native runtime; one-node lifecycle only |
| `ghcr.io/sylphxai/talos-installer:{talos-version}-host-memory-{source-revision}` | OCI image | Digest-pinned native runtime upgrade; never latest/fleet activation |

[cluster-api-provider-hetzner-robot](https://github.com/SylphxAI/cluster-api-provider-hetzner-robot)
downloads the disk image from:

```
https://github.com/SylphxAI/talos-images/releases/download/{talos-version}/metal-amd64.raw.zst
```

## Build

A push to `main` builds the default Talos version's separate native variant.
Manual dispatch without `native_host_memory` retains the stock/advanced path:

```bash
gh workflow run build.yml -R SylphxAI/talos-images -f talos_version=v1.14.1
```

To publish the opt-in stargz variant:

```bash
gh workflow run build.yml -R SylphxAI/talos-images -f talos_version=v1.14.1 -f stargz=true
```

This adds `siderolabs/stargz-snapshotter`, resolved with its digest from the
requested Talos version's factory catalog, alongside Kata. The release and
installer tag both gain `-stargz`; the variant does not replace the default
Kata-only artifacts or become the latest release. Its disk-image URL is:

```
https://github.com/SylphxAI/talos-images/releases/download/{talos-version}-stargz/metal-amd64.raw.zst
```

Node configuration and rollout remain the responsibility of downstream
infrastructure consumers; selecting this image alone does not enable lazy
pulling for workloads.

## Native runtime stack releases

The managed microVM pattern is Namespace/Depot: customers do not select an OS,
runtime or node pool. Foundation publishes an immutable runtime release and
rolls it through its existing one-node Talos-native lifecycle. Public advanced
lease overrides remain platform contracts, not infrastructure prerequisites.

The optional `native_runtime_release` input is one versioned JSON manifest:
`schema_version: 1`, `talos_version`, full `source_revision`, immutable `imager`
and `kata_extension`, and matching digest-pinned `kubelet` plus `kubelet_version`.
All three native components are required. The custom imager carries corrected
containerd core boot assets; an extension or installer-base override cannot
replace `/usr/bin/containerd`. The matching kubelet preserves the existing
`v1.36.4` ABI while choosing product best-effort memory victims before protected
services. Its version tag plus digest preserves Talos compatibility detection;
the digest selects the bytes. Source and native build live in
[`host-memory/`](host-memory/README.md), beside this immutable image builder.

Native artifacts use a source-revision-qualified `-host-memory-<revision>`
release/tag and never become latest or overwrite the normal Talos artifact.
The existing explicit Kata override and stargz mode remain available in manual
stock builds. A complete native manifest may be reused for advanced assembly,
but cannot be mixed with a separate Kata override.

Publication does not retarget a fleet, label a node or establish runtime safety.
Foundation verifies the exact installer and all native component identities,
upgrades one drained supported worker in a supervised run, records a new immutable
template without changing the shared MachineDeployment, then obtains independent
host accounting, reclaim and fail-alone pressure readback. Full guest requests
and the closed safety gate remain until that proof exists.

Every push to `main` builds the native source automatically as a separate
immutable variant. Manual `native_host_memory=true` rebuilds it explicitly.
The existing free standard hosted builder compiles pinned sources, preserves
Talos image verification and builds the core imager with a local native
containerd OCI context. Native component tags share the existing public
`ghcr.io/sylphxai/talos-installer` artifact repository and its existing publishing
identity; no new package visibility, borrowed key or registry-policy expansion
is needed. Other tags remain unchanged.

The workflow reads every digest back and publishes `native-runtime-release.json`
with the final installer identity alongside the metal image. Foundation passes
that manifest to its one-node renderer instead of hand-constructing a release.
Required CI runs deterministic boundary tests, compiles the actual corrected
executables, and reproduces the kubelet victim-order regression before applying
the correction, then proves it passing afterward. These are source/artifact
checks, not production host-pressure acceptance.

