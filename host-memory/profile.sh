#!/usr/bin/env bash
# Reviewed native ABI pairs. Source this before building; unsupported versions
# fail before downloading or publishing an artifact. DE remains the default.
case "${TALOS_VERSION:-v1.14.1}" in
  v1.14.1)
    TALOS_VERSION=v1.14.1
    TALOS_SHA256=49710f8a98f9c98f88453cc2d5ebbfe5c128183785684a9defbc60ad5c76dd55
    CONTAINERD_VERSION=2.3.5
    CONTAINERD_SHA256=a99a4dca98061064ff4cb35d27d1ec2345717e9108c822329fcec91dc72bff96
    CONTAINERD_BASE=ghcr.io/siderolabs/containerd@sha256:6737d2f431ac5c181aef7aac44cbbb87ac9370b6d34e5298ed0a47f9be8bc854
    KATA_BASE=ghcr.io/siderolabs/kata-containers@sha256:699b92efa132888bd286e29c096bbe412f73e5cdea07ce4c5c83b408a3000ad4
    ;;
  v1.14.2)
    TALOS_VERSION=v1.14.2
    TALOS_SHA256=00e51df1940b836fdbc148d8c263823669e20aa5f6424b3be15127c6cc89acc9
    CONTAINERD_VERSION=2.3.6
    CONTAINERD_SHA256=b4b407c64e11a22549b14c47857ac0e02e9f47f8cfc28924e9aa04a0adcaa883
    CONTAINERD_BASE=ghcr.io/siderolabs/containerd@sha256:c95f99188635260c992f2f5bae1e5bd3890d93e73ba1eb4e82df1874745be24f
    KATA_BASE=ghcr.io/siderolabs/kata-containers@sha256:803def92a6dbbef6746afdb68271b4c079ab10e0faec5c8be79dd2047db6b168
    ;;
  *) echo 'native runtime supports only reviewed Talos v1.14.1 and v1.14.2 pairs' >&2; return 1 ;;
esac
