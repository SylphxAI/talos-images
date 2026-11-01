package runtime

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type efiReadTestStream struct {
	grpc.ServerStream
	data []byte
}

func (s *efiReadTestStream) Context() context.Context { return context.Background() }
func (s *efiReadTestStream) Send(d *common.Data) error {
	s.data = append(s.data, d.Bytes...)
	return nil
}
func (s *efiReadTestStream) SendMsg(msg any) error { return s.Send(msg.(*common.Data)) }

func TestEFIInventoryRejectsCallerPathsBeforeAnyHostAccess(t *testing.T) {
	// A nil controller and stream ensure an injected path never reaches state,
	// device opening, mounting or output. This is the existing Read RPC/role.
	for _, path := range []string{"/dev/nvme0n1", "/dev/nvme0n1p4", "/etc/config", "../EFI"} {
		err := (&Server{}).Read(&machine.ReadRequest{Path: path, EfiInventory: true}, nil)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("path %q: %v", path, err)
		}
	}
}

func TestEFIInventoryLeavesOrdinaryReadRegularOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular")
	want := []byte("ordinary read still returns exact regular file bytes")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	stream := &efiReadTestStream{}
	if err := (&Server{}).Read(&machine.ReadRequest{Path: path}, stream); err != nil {
		t.Fatal(err)
	}
	if string(stream.data) != string(want) {
		t.Fatal("ordinary regular-file contents changed")
	}
	stream = &efiReadTestStream{}
	if err := (&Server{}).Read(&machine.ReadRequest{Path: "/dev/null"}, stream); err == nil || len(stream.data) != 0 {
		t.Fatal("generic Read accepted a nonregular device")
	}
}

func TestEFIInventoryDeviceDescriptorCannotWriteOrFollowSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-fixture")
	want := []byte("fixture bytes must survive an attempted write")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openEFIReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("overwrite")); err == nil {
		t.Fatal("inventory descriptor accepted a write")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatal("inventory open/write changed source bytes")
	}
	link := path + "-link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err = openEFIReadOnly(link); err == nil {
		f.Close()
		t.Fatal("inventory opener followed a symlink")
	}
}

func TestEFIInventoryDetachedMountFlagsBeforeTraversal(t *testing.T) {
	// Exercise every subset of the required kernel flags: only all four pass.
	bits := []int64{unix.ST_RDONLY, unix.ST_NOSUID, unix.ST_NODEV, unix.ST_NOEXEC}
	for subset := range 16 {
		var flags int64
		for i, bit := range bits {
			if subset&(1<<i) != 0 {
				flags |= bit
			}
		}
		set := func(fd int, path string, operation uint, attr *unix.MountAttr) error {
			if fd != 42 || path != "" || operation != unix.AT_EMPTY_PATH || attr.Attr_clr != 0 || attr.Attr_set != unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOEXEC {
				t.Fatal("mount operation escaped detached read-only boundary")
			}
			return nil
		}
		readFlags := func(_ int, stat *unix.Statfs_t) error { stat.Flags = flags; return nil }
		err := secureEFIInventoryMount(42, set, readFlags)
		if (err == nil) != (subset == 15) {
			t.Fatalf("accepted unsafe kernel flag subset %d: %v", subset, err)
		}
	}
	called := false
	err := secureEFIInventoryMount(42, func(int, string, uint, *unix.MountAttr) error { return errors.New("refused") }, func(int, *unix.Statfs_t) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("continued after attribute application refusal")
	}
}
