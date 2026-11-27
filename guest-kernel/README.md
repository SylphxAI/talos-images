# Guest kernel

The x86_64 kernel that Kata build leases boot comes from the pinned Kata source
and config in `build-kernel.sh`, with two local fragments:

- `qos.conf` builds in the `prio`, `htb`, `netem` and `flower` traffic-control options
  used by the etcd peer shaper.
- `nested-kvm.conf` builds in AMD and Intel KVM and vhost-net. The guest can
  create its own `/dev/kvm` when its VMM exposes SVM or VMX. This does not pass
  through the host's KVM device or change device permissions in workloads.

Kata disables kernel modules, so these settings must be built in. The recipe
checks the Kata and Linux source hashes and uses the upstream Kata config and
patches before merging the local fragments. Compiler tools and libraries are
vendored as a fallback for build leases without system packages.

```bash
guest-kernel/build-kernel.sh setup   # merge fragments and check the resulting .config
guest-kernel/build-kernel.sh build   # compile vmlinux
guest-kernel/build-kernel.sh verify  # check .config and linked vmlinux initcalls
```

On main pushes the Talos build installs this verified ELF as `vmlinux.container`
at the path already used by Kata. The build summary reports the published Kata
extension digest and the kernel's SHA-256 read back from that image. These are
artifact checks, not proof that a particular host and Kata lease can boot an
L2 VM.
