# syntax=docker/dockerfile:1.19
# Build native runtime binaries; never wrap or replace a runtime on a live host.
FROM golang:1.26.3-alpine AS native
RUN apk add --no-cache bash curl git make patch gcc musl-dev pkgconf
WORKDIR /source
COPY host-memory/ /source/host-memory/
RUN curl -fsSL https://codeload.github.com/containerd/containerd/tar.gz/refs/tags/v2.3.5 -o containerd.tar.gz \
 && echo 'a99a4dca98061064ff4cb35d27d1ec2345717e9108c822329fcec91dc72bff96  containerd.tar.gz' | sha256sum -c - \
 && curl -fsSL https://codeload.github.com/kata-containers/kata-containers/tar.gz/refs/tags/3.32.0 -o kata.tar.gz \
 && echo '722a022d873e6742788eca9f6ed74e8b7c4f70ffe4713144bd2e91fc3db620d0  kata.tar.gz' | sha256sum -c - \
 && tar -xzf containerd.tar.gz && tar -xzf kata.tar.gz \
 && sh host-memory/install.sh /source/containerd-2.3.5 /source/kata-containers-3.32.0
ARG SOURCE_REVISION
ENV CGO_ENABLED=0
RUN test "${#SOURCE_REVISION}" = 40 \
 && make -C /source/containerd-2.3.5 bin/containerd STATIC=1 VERSION=v2.3.5 REVISION="${SOURCE_REVISION}-host-memory" BUILDTAGS='no_aufs no_btrfs no_systemd no_zfs' \
 && PREFIX=/usr/local make -C /source/kata-containers-3.32.0/src/runtime SKIP_GO_VERSION_CHECK=y NATIVE_REVISION="${SOURCE_REVISION}" containerd-shim-v2 \
 && mkdir -p /out \
 && printf '%s\n' "${SOURCE_REVISION}" > /out/source-revision

# Node-pressure eviction must respect the same product tier even when kubelet's
# legacy pod-cgroup view reports zero guest usage. Keep its existing 1.36.4 ABI.
FROM golang:1.26.3-alpine AS native-kubelet
RUN apk add --no-cache bash curl git patch
WORKDIR /source
COPY host-memory/ /source/host-memory/
RUN curl -fsSL https://codeload.github.com/kubernetes/kubernetes/tar.gz/refs/tags/v1.36.4 -o kubernetes.tar.gz \
 && echo '86a216801ddba7098a50c6f3b3fe5fbc28ba34ea5b1daa66d81c2d5307c301ac  kubernetes.tar.gz' | sha256sum -c - \
 && tar -xzf kubernetes.tar.gz \
 && bash host-memory/install-kubelet.sh /source/kubernetes-1.36.4
ARG SOURCE_REVISION
ENV CGO_ENABLED=0
RUN test "${#SOURCE_REVISION}" = 40 && mkdir -p /out \
 && go -C /source/kubernetes-1.36.4 build -mod=vendor -trimpath \
    -ldflags "-X k8s.io/component-base/version.gitVersion=v1.36.4+hostmemory.${SOURCE_REVISION} -X k8s.io/component-base/version.gitCommit=${SOURCE_REVISION} -X k8s.io/component-base/version.gitMajor=1 -X k8s.io/component-base/version.gitMinor=36 -X k8s.io/component-base/version.gitTreeState=clean" \
    -o /out/kubelet ./cmd/kubelet

FROM ghcr.io/siderolabs/kubelet@sha256:fb1caca3bea28c83d733ebc25baa83cf8dc2fa3c89353c57dd666c23e1729829 AS kubelet-image
COPY --from=native-kubelet /out/kubelet /usr/local/bin/kubelet
ARG SOURCE_REVISION
LABEL org.opencontainers.image.source=https://github.com/SylphxAI/talos-images
LABEL org.opencontainers.image.revision=${SOURCE_REVISION}


# Retain the release-matched Talos package's tools and metadata. install.sh also
# preserves Sidero's native image-verification integration in the rebuilt binary.
FROM ghcr.io/siderolabs/containerd@sha256:6737d2f431ac5c181aef7aac44cbbb87ac9370b6d34e5298ed0a47f9be8bc854 AS containerd-package
COPY --from=native /source/containerd-2.3.5/bin/containerd /usr/bin/containerd
COPY --from=native /out/source-revision /usr/share/sylphx-host-memory/source-revision
ARG SOURCE_REVISION
LABEL org.opencontainers.image.revision=${SOURCE_REVISION}
LABEL org.opencontainers.image.source=https://github.com/SylphxAI/talos-images

# All guest/VMM assets come from the Talos1.14.1 factory's exact Kata3.32.0
# extension. Only the native shim and its explicit release identity change.
FROM ghcr.io/siderolabs/kata-containers@sha256:699b92efa132888bd286e29c096bbe412f73e5cdea07ce4c5c83b408a3000ad4 AS kata-extension
COPY --from=native /source/kata-containers-3.32.0/src/runtime/containerd-shim-kata-v2 /rootfs/usr/local/bin/containerd-shim-kata-v2
COPY --from=native /out/source-revision /rootfs/usr/local/share/kata-containers/host-memory-source-revision
ARG SOURCE_REVISION
LABEL org.opencontainers.image.revision=${SOURCE_REVISION}
LABEL org.opencontainers.image.source=https://github.com/SylphxAI/talos-images
