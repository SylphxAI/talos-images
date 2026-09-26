# Talos Images

<p align="center">
  <img src="https://mark.sylphx.com/api/v1/mark/hero?type=constellation&theme=tokyonight&text=talos+images&desc=Custom+Talos+Linux+image+composition%3A+declares+system+extensions+and+publishes+metal+install%E2%80%A6&height=200&animation=rise" alt="talos-images — Sylphx Mark banner" width="100%" />
</p>

[Talos Linux](https://www.talos.dev) images for bare-metal servers, built with
the [Kata Containers](https://katacontainers.io) system extension so pods can
run in lightweight virtual machines.

The build looks up the Kata extension that matches the requested Talos version
in the [Sidero image factory](https://factory.talos.dev) catalog, so a new
Talos version needs no edit here. [extensions.yaml](extensions.yaml) explains
the selection rules.

## Outputs

| Artifact | Format | Use |
|----------|--------|-----|
| `metal-amd64.raw.zst`, on the [release](https://github.com/SylphxAI/talos-images/releases) named after the Talos version | Raw disk image | First install: write it to the server's disk |
| `ghcr.io/sylphxai/talos-installer:{talos-version}` | OCI image | `talosctl upgrade --image ...` |

[cluster-api-provider-hetzner-robot](https://github.com/SylphxAI/cluster-api-provider-hetzner-robot)
downloads the disk image from:

```
https://github.com/SylphxAI/talos-images/releases/download/{talos-version}/metal-amd64.raw.zst
```

## Build

A push to `main` builds the default Talos version. To build another version:

```bash
gh workflow run build.yml -R SylphxAI/talos-images -f talos_version=v1.14.1
```
