# Installed EFI inventory

`talosctl efi-inventory` is an on-demand, metadata-only read of the installed
EFI filesystem. Both client and machined must include this source integration.
A stock server refuses the request; this does not prove anything about its EFI
files. The feature does not supply a way to install itself on an unreachable or
unqualified node.

The explicit `ReadRequest.efi_inventory` mode reuses the existing authenticated
Read RPC and authorization. It accepts no path or device. Ordinary Read stays
regular-file-only. No service, port, credential, grant, controller or scheduled
process is added.

Machined selects exactly one `EFI` partition from its authoritative system-disk
and discovered-volume resources. Parent disk, GPT ESP type, UUID, index, vfat
filesystem and size must agree. Kernel sysfs parent/index/size and opened block
major/minor are checked. Only that device is opened, with `O_RDONLY|O_NOFOLLOW`;
the parent disk and data partitions are never opened by this operation.

The existing Talos fsopen primitive creates a **detached** vfat mount with
read-only, nodev, nosuid and noexec attributes. It never attaches a host path,
repairs a filesystem or changes a boot variable. Descriptors are released when
the operation returns; the detached mount is closed before a successful response.
This source has not been exercised on a live node.
Talos's generic mount-attribute helpers skip detached mounts, so this operation
sets attributes directly on its detached mount FD and verifies all four flags
with `fstatfs` before traversing files. Its initial fsopen configuration is also
read-only. Missing or refused flags fail without inventory output.

The response contains schema version, disk/partition identity, boot ID and booted
EFI filename, plus each file's relative path, size and SHA256. It returns no UKI
body, command line, configuration, certificate or secret values. The client
rejects unknown fields, trailing documents and excess output before printing.
The result explicitly says `snapshot_atomic: false`: files can change during a
separate upgrade, so inventory alone never asserts a usable fallback or restore.

Bounds are one concurrent request per machined; a cooperative 110-second deadline;
3 GiB of file bytes at 32 MiB/s; 4,096 entries including directories; directory
depth 8; 255-byte filenames; 2 MiB of output; and an 8 GiB EFI partition. Directory
reads are batched, hashing uses 64 KiB buffers, and a refusal returns no partial
inventory. Symlinks, special files, changing/short files and ambiguous identity
refuse. Kernel I/O itself can block beyond a cooperative context deadline.

`install.sh` integrates only into hash-pinned Talos v1.14.1 source (2f86b9d2).
Generated protobuf and vtprotobuf code is included in the patch; source and wire
tests check the new mode and legacy requests. `check.sh` checks identity, traversal,
read/output/write boundaries, integration refusal before mutation and existing
sdboot behavior. It does not build/publish an image or perform a live operation.

```sh
bash boot-inventory/check.sh
```

At 10x/100x request rate the per-node concurrency bound refuses excess requests;
there is no queue or resident worker. Large EFI populations hit explicit byte,
entry, output or deadline bounds rather than consuming unbounded resources.
Within the source-preparation horizon, a wrong-partition or writable mount is
qualitatively possible/major (3x4=12). Authoritative identity selection, kernel
cross-checks, a read-only descriptor/detached mount and negative tests reduce the
source risk; actual device/mount behavior remains unverified until separately
authorized artifact and node acceptance. Data-preservation rules still apply.
