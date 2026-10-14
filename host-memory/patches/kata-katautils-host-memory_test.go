package katautils

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/kata-containers/kata-containers/src/runtime/pkg/oci"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
)

func nativeTestDigest(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func TestNativeParentAcceptsOnlyAdmittedPrivateChildren(t *testing.T) {
	be := "/sylphx-besteffort/pod-" + nativeTestDigest("sb")
	if parent, bestEffort, err := NativeParent(be+"/sb", "sb"); err != nil || !bestEffort || parent != be {
		t.Fatalf("best-effort: %q %v %v", parent, bestEffort, err)
	}
	protected := "/kubepods/burstable/poda/native-" + nativeTestDigest("sb")
	if parent, bestEffort, err := NativeParent(protected+"/sb", "sb"); err != nil || bestEffort || parent != protected {
		t.Fatalf("protected: %q %v %v", parent, bestEffort, err)
	}
	for _, path := range []string{
		"/kubepods/burstable/poda/sb",
		"/kubepods/native-" + nativeTestDigest("sb") + "/sb",
		"/system/native-" + nativeTestDigest("sb") + "/sb",
		"/sylphx-besteffort/pod-" + nativeTestDigest("other") + "/sb",
		protected + "/../x/sb",
		"kubepods.slice:cri-containerd:sb",
		"",
	} {
		if _, _, err := NativeParent(path, "sb"); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestNativeSandboxRequiresFreshLocalSandboxOnlyVM(t *testing.T) {
	path := "/sylphx-besteffort/pod-" + nativeTestDigest("sb") + "/sb"
	valid := vc.SandboxConfig{SandboxCgroupOnly: true, HypervisorType: vc.ClhHypervisor}
	if err := ValidateNativeSandbox(path, "sb", valid); err != nil {
		t.Fatal(err)
	}
	escapes := map[string]func(*vc.SandboxConfig){
		"host overhead cgroup": func(c *vc.SandboxConfig) { c.SandboxCgroupOnly = false },
		"remote VM":            func(c *vc.SandboxConfig) { c.HypervisorType = vc.RemoteHypervisor },
		"firecracker":          func(c *vc.SandboxConfig) { c.HypervisorType = vc.FirecrackerHypervisor },
		"jailer":               func(c *vc.SandboxConfig) { c.HypervisorConfig.JailerPath = "/usr/bin/jailer" },
		"template boot":        func(c *vc.SandboxConfig) { c.HypervisorConfig.BootFromTemplate = true },
		"template build":       func(c *vc.SandboxConfig) { c.HypervisorConfig.BootToBeTemplate = true },
		"factory VM":           func(c *vc.SandboxConfig) { c.HypervisorConfig.VMid = "cached" },
	}
	for name, mutate := range escapes {
		config := valid
		mutate(&config)
		if err := ValidateNativeSandbox(path, "sb", config); err == nil {
			t.Fatalf("%s escaped the native envelope", name)
		}
	}
	if err := ValidateNativeSandbox("/kubepods/poda/sb", "sb", valid); err == nil {
		t.Fatal("sandbox outside its private parent accepted")
	}
}

func TestNativeFactoryPathsAreRefused(t *testing.T) {
	if err := ValidateNativeFactory(oci.FactoryConfig{TemplatePath: "/run/vc/vm/template"}); err != nil {
		t.Fatal(err)
	}
	for _, config := range []oci.FactoryConfig{{Template: true}, {VMCacheNumber: 1}, {VMCacheEndpoint: "/run/kata/cache.sock"}} {
		if err := ValidateNativeFactory(config); err == nil {
			t.Fatalf("factory path %+v accepted", config)
		}
	}
}
