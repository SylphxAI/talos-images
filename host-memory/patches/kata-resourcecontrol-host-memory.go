package resourcecontrol

// NativeShimLeaf is the process leaf below the runtime-owned sandbox parent.
// A domain parent with delegated controllers cannot itself receive shim tasks.
const NativeShimLeaf = ".native-shim"
