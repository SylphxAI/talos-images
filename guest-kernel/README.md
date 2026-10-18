# Guest kernel

The kernel a build lease boots. It is Kata's guest kernel for the release
pinned in `build-kernel.sh`, with one addition: the traffic-control options
the etcd peer shaper uses (`prio`, `htb`, `netem`, `flower`) are built in.

The stock kernel leaves them out, and it is built without module support, so
they cannot be loaded afterwards. `tc qdisc add ... prio` then fails with
"Specified qdisc kind is unknown", and the shaper cannot run inside a lease.

`qos.conf` is that addition. `build-kernel.sh` follows Kata's own build
(`tools/packaging/kernel/build-kernel.sh`): the same kernel version, the same
fragment merge, then this fragment on top so it wins. It checks the kernel
version and config version it finds in the Kata source against the pins, and
refuses to build if they differ. The compiler tools a lease image lacks
(`m4`, `flex`, `bison`, `bc`, `ed`, `zlib`, `elfutils`) are vendored under
`vendor/` with their checksums, because a lease has no root and no route to a
package archive.

```bash
guest-kernel/build-kernel.sh setup     # config, then checks the options are =y
guest-kernel/build-kernel.sh build     # vmlinux
guest-kernel/build-kernel.sh symbols   # the four qdiscs are registered
```

Run it on a build lease, not the desk. The lease needs `github.com`,
`codeload.github.com` and `cdn.kernel.org` allowed for the two source
archives. The compile takes about ten minutes on a large lease.
