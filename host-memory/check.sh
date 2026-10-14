#!/usr/bin/env bash
# Run only in an owned remote Build lease or CI runner; never on the desk.
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_revision=${SOURCE_SHA:-$(git -C "$root" rev-parse HEAD)}
[[ "$source_revision" =~ ^[a-f0-9]{40}$ ]] || { echo 'full native source revision required' >&2; exit 2; }
unformatted=$(gofmt -l "$root/guard" "$root/containerd" "$root/patches" "$root/kubelet")
if [ -n "$unformatted" ]; then printf 'Unformatted native sources:\n%s\n' "$unformatted" >&2; exit 1; fi
nice -n 19 ionice -c3 go -C "$root/guard" test ./... -count=1
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
curl -fsSL --max-time 120 https://codeload.github.com/containerd/containerd/tar.gz/refs/tags/v2.3.5 -o "$scratch/containerd.tar.gz"
curl -fsSL --max-time 120 https://codeload.github.com/kata-containers/kata-containers/tar.gz/refs/tags/3.32.0 -o "$scratch/kata.tar.gz"
printf '%s  %s\n' a99a4dca98061064ff4cb35d27d1ec2345717e9108c822329fcec91dc72bff96 "$scratch/containerd.tar.gz" 722a022d873e6742788eca9f6ed74e8b7c4f70ffe4713144bd2e91fc3db620d0 "$scratch/kata.tar.gz" | sha256sum -c -
tar -xzf "$scratch/containerd.tar.gz" -C "$scratch"
tar -xzf "$scratch/kata.tar.gz" -C "$scratch"
nice -n 19 ionice -c3 "$root/install.sh" "$scratch/containerd-2.3.5" "$scratch/kata-containers-3.32.0"
export CGO_ENABLED=0
nice -n 19 ionice -c3 go -C "$scratch/containerd-2.3.5" test -tags no_aufs,no_btrfs,no_systemd,no_zfs ./internal/cri/server -run '^TestNative' -count=1
nice -n 19 ionice -c3 make -C "$scratch/kata-containers-3.32.0/src/runtime" pkg/katautils/config-settings.go PREFIX=/usr/local SKIP_GO_VERSION_CHECK=y "NATIVE_REVISION=$source_revision"
nice -n 19 ionice -c3 go -C "$scratch/kata-containers-3.32.0/src/runtime" test ./pkg/katautils ./pkg/containerd-shim-v2 -run '^TestNative' -count=1
nice -n 19 ionice -c3 make -C "$scratch/containerd-2.3.5" bin/containerd STATIC=1 VERSION=v2.3.5 "REVISION=$source_revision-host-memory" BUILDTAGS='no_aufs no_btrfs no_systemd no_zfs'
nice -n 19 ionice -c3 make -C "$scratch/kata-containers-3.32.0/src/runtime" containerd-shim-v2 PREFIX=/usr/local SKIP_GO_VERSION_CHECK=y "NATIVE_REVISION=$source_revision"
# Actual corrected executables, not source-text assertions or mocked wrappers.
"$scratch/containerd-2.3.5/bin/containerd" --version
"$scratch/kata-containers-3.32.0/src/runtime/containerd-shim-kata-v2" --version

curl -fsSL --max-time 120 https://codeload.github.com/kubernetes/kubernetes/tar.gz/refs/tags/v1.36.4 -o "$scratch/kubernetes.tar.gz"
printf '%s  %s\n' 86a216801ddba7098a50c6f3b3fe5fbc28ba34ea5b1daa66d81c2d5307c301ac "$scratch/kubernetes.tar.gz" | sha256sum -c -
tar -xzf "$scratch/kubernetes.tar.gz" -C "$scratch"
kubernetes="$scratch/kubernetes-1.36.4"
test ! -e "$kubernetes/pkg/kubelet/eviction/native_memory_test.go"
cp "$root/kubelet/native_memory_test.go" "$kubernetes/pkg/kubelet/eviction/native_memory_test.go"
set +e
nice -n 19 ionice -c3 go -C "$kubernetes" test -mod=vendor ./pkg/kubelet/eviction -run '^TestNativeMemoryPressure' -count=1 -json > "$scratch/baseline.json"
baseline_status=$?
set -e
test "$baseline_status" -ne 0
# The negative control must reach the actual consumer-visible test failure,
# not merely fail to fetch dependencies or compile the upstream package.
python3 - "$scratch/baseline.json" <<'PY'
import json
import sys
events = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
if not any(event.get("Action") == "fail" and event.get("Test") == "TestNativeMemoryPressureProtectsServicesAtFullRequests" for event in events):
    raise SystemExit("upstream negative control did not reach the intended eviction-order regression")
print("upstream memory victim-order regression reproduced")
PY
rm -- "$kubernetes/pkg/kubelet/eviction/native_memory_test.go"
"$root/install-kubelet.sh" "$kubernetes"
nice -n 19 ionice -c3 go -C "$kubernetes" test -mod=vendor ./pkg/kubelet/eviction -run '^TestNativeMemoryPressure' -count=1
nice -n 19 ionice -c3 go -C "$kubernetes" build -mod=vendor -trimpath \
  -ldflags "-X k8s.io/component-base/version.gitVersion=v1.36.4+hostmemory.$source_revision -X k8s.io/component-base/version.gitCommit=$source_revision -X k8s.io/component-base/version.gitMajor=1 -X k8s.io/component-base/version.gitMinor=36 -X k8s.io/component-base/version.gitTreeState=clean" \
  -o "$scratch/native-kubelet" ./cmd/kubelet
"$scratch/native-kubelet" --version
