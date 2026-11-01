package bootinventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func noPace(context.Context, time.Duration, int64) error { return nil }

func validVolume() Volume {
	return Volume{ID: "nvme0n1p1", Path: "/dev/nvme0n1p1", ParentID: "nvme0n1", ParentPath: "/dev/nvme0n1", Type: "partition", Label: "EFI", PartitionType: EFIPartitionType, UUID: "11111111-2222-3333-4444-555555555555", Filesystem: "vfat", Index: 1, Bytes: 2203058176}
}

func TestSelectOnlyAuthoritativeEFI(t *testing.T) {
	good := validVolume()
	data := good
	data.ID = "nvme0n1p4"
	data.Label = "STATE"
	data.PartitionType = "linux"
	other := good
	other.ID = "nvme1n1p1"
	other.ParentID = "nvme1n1"
	other.ParentPath = "/dev/nvme1n1"
	other.Path = "/dev/" + other.ID
	got, err := SelectEFI("nvme0n1", "/dev/nvme0n1", []Volume{data, other, good})
	if err != nil || got != good {
		t.Fatalf("wrong selection: %+v %v", got, err)
	}
	tests := map[string]func(*Volume){
		"wrong GPT type":    func(v *Volume) { v.PartitionType = "linux" },
		"data label":        func(v *Volume) { v.Label = "STATE" },
		"wrong parent ID":   func(v *Volume) { v.ParentID = "nvme1n1" },
		"wrong parent path": func(v *Volume) { v.ParentPath = "/dev/nvme1n1" },
		"path injection":    func(v *Volume) { v.Path = "/dev/nvme0n1p1/../../etc/shadow" },
		"ID injection":      func(v *Volume) { v.ID = "../nvme0n1p1" },
		"whole disk":        func(v *Volume) { v.Type = "disk" },
		"no UUID":           func(v *Volume) { v.UUID = "" },
		"zero size":         func(v *Volume) { v.Bytes = 0 },
		"oversize":          func(v *Volume) { v.Bytes = 8<<30 + 1 },
		"no index":          func(v *Volume) { v.Index = 0 },
		"wrong filesystem":  func(v *Volume) { v.Filesystem = "xfs" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			v := good
			mutate(&v)
			if _, err := SelectEFI("nvme0n1", "/dev/nvme0n1", []Volume{v}); err == nil {
				t.Fatal("accepted inconsistent identity")
			}
		})
	}
	for _, volumes := range [][]Volume{nil, {good, good}} {
		if _, err := SelectEFI("nvme0n1", "/dev/nvme0n1", volumes); err == nil {
			t.Fatal("accepted missing/ambiguous EFI")
		}
	}
	if _, err := SelectEFI("nvme1n1", "/dev/nvme0n1", []Volume{good}); err == nil {
		t.Fatal("accepted inconsistent system disk")
	}
}

func TestScanHashesWithoutReturningOrWritingContents(t *testing.T) {
	secret := []byte("embedded cmdline and configuration values must never leave the reader")
	filesystem := fstest.MapFS{"EFI/Linux/Talos-v1.14.1.efi": {Data: append([]byte{}, secret...)}, "EFI/boot/BOOTX64.efi": {Data: []byte("loader")}, "empty": {Data: []byte{}}}
	before := append([]byte{}, filesystem["EFI/Linux/Talos-v1.14.1.efi"].Data...)
	files, n, err := scan(context.Background(), filesystem, noPace)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || n != int64(len(secret)+6) {
		t.Fatalf("wrong accounting: %+v %d", files, n)
	}
	var matched bool
	for _, f := range files {
		if f.Path == "/EFI/Linux/Talos-v1.14.1.efi" {
			sum := sha256.Sum256(secret)
			matched = f.SHA256 == hex.EncodeToString(sum[:]) && f.Bytes == int64(len(secret))
		}
	}
	if !matched {
		t.Fatal("missing exact UKI size/hash")
	}
	data, _ := json.Marshal(files)
	if bytes.Contains(data, secret) {
		t.Fatal("file contents escaped")
	}
	if !bytes.Equal(before, filesystem["EFI/Linux/Talos-v1.14.1.efi"].Data) {
		t.Fatal("inventory changed source bytes")
	}
}

func TestTraversalRefusalsReturnNoPartialInventory(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"symlink":      {"safe": {Data: []byte("ok")}, "symlink": {Mode: fs.ModeSymlink, Data: []byte("../../data")}},
		"special file": {"special": {Mode: fs.ModeDevice}},
		"control name": {"bad\nname": {Data: []byte("bad")}},
		"format name":  {"bad\u202ename": {Data: []byte("bad")}},
		"too deep":     {strings.Repeat("d/", MaxDepth+1) + "file": {Data: []byte("bad")}},
		"long name":    {strings.Repeat("x", 256): {Data: []byte("bad")}},
	}
	population := fstest.MapFS{}
	for i := range MaxFiles + 1 {
		population[strings.Repeat("a", i/100)+string(rune(0x400+i%100))] = &fstest.MapFile{Data: []byte{}}
	}
	tests["too many entries"] = population
	for name, filesystem := range tests {
		t.Run(name, func(t *testing.T) {
			files, _, err := scan(context.Background(), filesystem, noPace)
			if err == nil || files != nil {
				t.Fatalf("unsafe partial result: %v %v", files, err)
			}
		})
	}
}

func TestCancellationAndReadBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if files, _, err := Scan(ctx, fstest.MapFS{"file": {Data: []byte("data")}}); err == nil || files != nil {
		t.Fatal("cancellation accepted")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, _, err := scan(ctx, fstest.MapFS{"file": {Data: bytes.Repeat([]byte("x"), 128<<10)}}, func(context.Context, time.Duration, int64) error { cancel(); return ctx.Err() })
	if err == nil {
		t.Fatal("mid-file cancellation accepted")
	}
	big := oversizeFS{MapFS: fstest.MapFS{"file": {Data: []byte("x")}}}
	if files, _, err := scan(context.Background(), big, noPace); err == nil || files != nil {
		t.Fatal("oversize file accepted")
	}
}

// Exaggerate metadata without allocating gigabytes; opening its body is forbidden.
type oversizeFS struct{ fstest.MapFS }

func (f oversizeFS) Open(name string) (fs.File, error) {
	if name != "." {
		panic("opened file beyond read budget")
	}
	v, e := f.MapFS.Open(name)
	return oversizedDir{v.(fs.ReadDirFile)}, e
}

type oversizedDir struct{ fs.ReadDirFile }

func (d oversizedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 || n > 32 {
		panic("unbounded directory read")
	}
	entries, e := d.ReadDirFile.ReadDir(n)
	for i, v := range entries {
		entries[i] = oversizedEntry{v}
	}
	return entries, e
}

type oversizedEntry struct{ fs.DirEntry }

func (e oversizedEntry) Info() (fs.FileInfo, error) {
	v, err := e.DirEntry.Info()
	return oversizedInfo{v}, err
}

type oversizedInfo struct{ fs.FileInfo }

func (oversizedInfo) Size() int64 { return MaxReadBytes + 1 }

func validResult() Result {
	return Result{SchemaVersion: 1, SystemDisk: "/dev/nvme0n1", Partition: "/dev/nvme0n1p1", PartitionUUID: validVolume().UUID, PartitionBytes: validVolume().Bytes, BootID: "11111111-2222-3333-4444-555555555555", BootedEntry: "Talos-v1.14.1.efi", Files: []File{}}
}

func TestMetadataOnlyOutputContract(t *testing.T) {
	r := validResult()
	encoded, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(bytes.NewReader(encoded))
	if err != nil || !reflect.DeepEqual(r, got) {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	for name, data := range map[string][]byte{
		"unknown body field":  bytes.Replace(encoded, []byte(`"schema_version":1`), []byte(`"schema_version":1,"raw_uki":"SECRET"`), 1),
		"second document":     append(append([]byte{}, encoded...), encoded...),
		"trailing junk":       append(append([]byte{}, encoded...), []byte("secret")...),
		"oversize whitespace": append(append([]byte{}, encoded...), bytes.Repeat([]byte(" "), MaxOutputBytes)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("accepted forbidden output")
			}
		})
	}
	for _, mutate := range []func(*Result){func(r *Result) { r.BootedEntry = "/etc/shadow" }, func(r *Result) { r.SnapshotAtomic = true }, func(r *Result) { r.ReadBytes = 1 }, func(r *Result) { r.Partition = r.SystemDisk }, func(r *Result) { r.Files = []File{{Path: "/../data", Bytes: 0, SHA256: strings.Repeat("0", 64)}} }} {
		r := validResult()
		mutate(&r)
		if data, err := Encode(r); err == nil || data != nil {
			t.Fatal("accepted invalid metadata")
		}
	}
	if _, err := Decode(io.LimitReader(bytes.NewReader(encoded), 2)); err == nil {
		t.Fatal("accepted truncated JSON")
	}
}
