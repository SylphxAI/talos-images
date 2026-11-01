package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mountv3 "github.com/siderolabs/talos/internal/pkg/mount/v3"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/bootinventory"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/xfs/fsopen"
)

// One request per machined, on demand. No controller, service or listening port.
var efiInventorySlot = make(chan struct{}, 1)

func (s *Server) readEFIInventory(in *machine.ReadRequest, stream machine.MachineService_ReadServer) (err error) {
	if in.Path != "" {
		return status.Error(codes.InvalidArgument, "EFI inventory does not accept a path")
	}
	select {
	case efiInventorySlot <- struct{}{}:
		defer func() { <-efiInventorySlot }()
	default:
		return status.Error(codes.ResourceExhausted, "EFI inventory already running")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), 110*time.Second)
	defer cancel()
	st := s.Controller.Runtime().State().V1Alpha2().Resources()
	disk, err := block.GetSystemDisk(ctx, st)
	if err != nil {
		return err
	}
	if disk == nil {
		return status.Error(codes.FailedPrecondition, "system disk unavailable")
	}
	volumes, err := safe.StateListAll[*block.DiscoveredVolume](ctx, st)
	if err != nil {
		return err
	}
	var candidates []bootinventory.Volume
	for v := range volumes.All() {
		candidates = append(candidates, inventoryVolume(v))
	}
	selected, err := bootinventory.SelectEFI(disk.DiskID, disk.DevPath, candidates)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	before, err := safe.StateGetByID[*block.DiscoveredVolume](ctx, st, selected.ID)
	if err != nil {
		return err
	}
	if inventoryVolume(before) != selected {
		return errors.New("EFI identity changed before opening")
	}
	bootID, err := safe.StateGetByID[*runtimeres.BootID](ctx, st, runtimeres.BootIDID)
	if err != nil {
		return err
	}
	booted, err := safe.StateGetByID[*runtimeres.BootedEntry](ctx, st, runtimeres.BootedEntryID)
	if err != nil {
		return err
	}
	if err = bootinventory.ValidateBootMetadata(bootID.TypedSpec().BootID, booted.TypedSpec().BootedEntry); err != nil {
		return err
	}
	device, err := openInventoryEFI(disk.DiskID, selected)
	if err != nil {
		return err
	}
	defer device.Close() //nolint:errcheck
	// Use the existing fsopen/mount primitive, detached and read-only from first
	// mount creation. It never attaches a path in the host mount namespace, and
	// never invokes filesystem repair, remount-rw, blkid or lifecycle methods.
	manager := mountv3.NewManager(mountv3.WithDetached(), mountv3.WithReadOnly(), mountv3.WithNoExec(), mountv3.WithSecure(), mountv3.WithFsopen("vfat", fsopen.WithSource(fmt.Sprintf("/proc/self/fd/%d", device.Fd())), fsopen.WithBoolParameter("ro")))
	point, err := manager.Mount()
	if err != nil {
		return errors.New("cannot open detached read-only EFI filesystem")
	}
	released := false
	defer func() {
		if !released {
			err = errors.Join(err, manager.Unmount())
		}
	}()
	mountFD, err := point.Root().Fd()
	if err != nil {
		return err
	}
	if err = secureEFIInventoryMount(mountFD, unix.MountSetattr, unix.Fstatfs); err != nil {
		return err
	}
	files, readBytes, err := bootinventory.Scan(ctx, point.Root())
	if err != nil {
		return err
	}
	after, err := safe.StateGetByID[*block.DiscoveredVolume](ctx, st, selected.ID)
	if err != nil {
		return err
	}
	diskAfter, err := block.GetSystemDisk(ctx, st)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before.TypedSpec(), after.TypedSpec()) || !reflect.DeepEqual(disk, diskAfter) {
		return errors.New("system disk/EFI identity changed during inventory")
	}
	result := bootinventory.Result{SchemaVersion: 1, SystemDisk: disk.DevPath, Partition: selected.Path, PartitionUUID: selected.UUID, PartitionBytes: selected.Bytes, BootID: bootID.TypedSpec().BootID, BootedEntry: booted.TypedSpec().BootedEntry, Files: files, ReadBytes: readBytes, SnapshotAtomic: false}
	data, err := bootinventory.Encode(result)
	if err != nil {
		return err
	}
	// Release the detached mount successfully before sending any acceptance data.
	if err = manager.Unmount(); err != nil {
		return err
	}
	released = true
	return stream.Send(&common.Data{Bytes: data})
}

// openInventoryEFI validates kernel parent, partition number, size and device
// identity, then opens only that block node with O_RDONLY|O_NOFOLLOW. It never
// opens the parent disk, another partition, or a caller-supplied path.
func openInventoryEFI(diskID string, v bootinventory.Volume) (*os.File, error) {
	sysPath := filepath.Join("/sys/class/block", v.ID)
	actual, err := filepath.EvalSymlinks(sysPath)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", diskID))
	if err != nil {
		return nil, err
	}
	if filepath.Dir(actual) != parent {
		return nil, errors.New("EFI kernel parent differs from system disk")
	}
	readNumber := func(name string) (uint64, error) {
		f, e := os.Open(filepath.Join(sysPath, name))
		if e != nil {
			return 0, e
		}
		defer f.Close() //nolint:errcheck
		b, e := io.ReadAll(io.LimitReader(f, 129))
		if e != nil {
			return 0, e
		}
		if len(b) > 128 {
			return 0, errors.New("invalid sysfs metadata length")
		}
		return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	number, err := readNumber("partition")
	if err != nil {
		return nil, err
	}
	sectors, err := readNumber("size")
	if err != nil {
		return nil, err
	}
	if number != uint64(v.Index) || sectors > (8<<30)/512 || sectors*512 != v.Bytes {
		return nil, errors.New("EFI kernel partition identity/size differs")
	}
	dev, err := os.ReadFile(filepath.Join(sysPath, "dev"))
	if err != nil {
		return nil, err
	}
	majorText, minorText, ok := strings.Cut(strings.TrimSpace(string(dev)), ":")
	if !ok {
		return nil, errors.New("invalid kernel device identity")
	}
	major, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return nil, err
	}
	minor, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return nil, err
	}
	f, err := openEFIReadOnly(v.Path)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFBLK || unix.Major(stat.Rdev) != uint32(major) || unix.Minor(stat.Rdev) != uint32(minor) {
		f.Close()
		return nil, errors.New("opened EFI block identity differs")
	}
	return f, nil
}

func openEFIReadOnly(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

// Talos Point.Mount and its attribute setters intentionally skip detached
// mounts. Set attributes on this detached FD directly, then verify the kernel
// view before any file traversal. The fsopen "ro" parameter makes initial
// filesystem creation read-only, including the interval before this check.
func secureEFIInventoryMount(fd int, set func(int, string, uint, *unix.MountAttr) error, statfs func(int, *unix.Statfs_t) error) error {
	attributes := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC}
	if err := set(fd, "", unix.AT_EMPTY_PATH, attributes); err != nil {
		return errors.New("cannot secure detached EFI mount")
	}
	var stat unix.Statfs_t
	if err := statfs(fd, &stat); err != nil {
		return errors.New("cannot verify detached EFI mount flags")
	}
	required := int64(unix.ST_RDONLY | unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC)
	if stat.Flags&required != required {
		return errors.New("EFI mount is missing read-only/security flags")
	}
	return nil
}

func inventoryVolume(v *block.DiscoveredVolume) bootinventory.Volume {
	p := v.TypedSpec()
	return bootinventory.Volume{ID: v.Metadata().ID(), Path: p.DevPath, ParentID: p.Parent, ParentPath: p.ParentDevPath, Type: p.Type, Label: p.PartitionLabel, PartitionType: p.PartitionType, UUID: p.PartitionUUID, Filesystem: p.Name, Index: p.PartitionIndex, Bytes: p.Size}
}
