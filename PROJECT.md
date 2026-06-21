# talos-images

`talos-images` is an active integration repository for composing and publishing
custom Talos Linux images used by Sylphx Platform infrastructure. It owns the
image extension set, build workflow, release artifact, and installer image
surface that downstream cluster provisioning or upgrade flows consume.

## Lifecycle And Layer

- Lifecycle: `active`
- Layer: `integration`

## Goals

- Declare the Talos system extension set used to build custom Talos images.
- Build metal install artifacts and installer images from explicit Talos version
  and extension inputs.
- Publish release and GHCR artifacts that downstream provisioning and upgrade
  systems can consume through documented references.

## Non-Goals

- Own individual system extension implementations, Talos Linux upstream
  behavior, or Sidero imager internals.
- Own cluster rollout orchestration, node upgrade sequencing, or Kubernetes
  workload safety policy.
- Publish enterprise doctrine, org rulesets, or shared CI/release policy.

## Boundaries

This repository owns custom Talos image composition and artifact publication.
It consumes extension images through documented references only. Cluster
provisioning, upgrade decisions, and node runtime recovery belong to downstream
infrastructure controllers or runbooks.

## Public Surfaces

- `README.md` documents image outputs and rebuild usage.
- `extensions.yaml` is the declared system extension set.
- `.github/workflows/build.yml` builds the metal image, installer image, GitHub
  Release artifact, and GHCR installer image.
- GitHub Releases expose `metal-amd64.raw.zst`.
- GHCR exposes `ghcr.io/sylphxai/talos-installer:{version}`.
- `.doctrine/project.json` is the machine-readable project manifest.

## Delivery

Main pushes and manual dispatch build and publish image artifacts. Pull-request
admission is not yet declared in this repository. Production proof for image
changes must include workflow success, release artifact readback, installer
image readback, and install or upgrade smoke evidence in the intended Talos
environment.

The authoritative control-plane record is `.doctrine/project.json`.
