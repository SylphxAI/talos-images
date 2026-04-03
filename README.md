# Talos Images

Custom Talos Linux images for Sylphx Platform infrastructure.

Combines the base Talos OS with system extensions:
- `siderolabs/kata-containers` — Kata Containers runtime (Cloud Hypervisor)
- `sylphxai/talos-devmapper-pool` — dm-thin-pool bootstrap for containerd devmapper snapshotter

## Outputs

| Artifact | Format | Purpose |
|----------|--------|---------|
| `metal-amd64.raw.zst` | Raw disk image | CAPHR initial install (dd to disk) |
| `ghcr.io/sylphxai/talos-installer:{version}` | OCI image | `talosctl upgrade` |

## Usage

CAPHR downloads the metal image from GitHub Releases:
```
https://github.com/SylphxAI/talos-images/releases/download/{version}/metal-amd64.raw.zst
```

## Rebuild

Trigger manually via `workflow_dispatch` or push a tag:
```bash
gh workflow run build.yml -f talos_version=v1.12.6
```
