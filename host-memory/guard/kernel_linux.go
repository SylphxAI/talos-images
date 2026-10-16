//go:build linux

package guard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const cgroup2Magic = 0x63677270
const reclaimWorkerArgument = "--sylphx-host-memory-reclaim-fd"
const reclaimWorkerEnvironment = "SYLPHX_HOST_MEMORY_RECLAIM_WORKER"

// The package runs a narrowly scoped one-shot helper only when this exact
// private argv/environment pair is supplied. The native executable imports
// this package, so no additional daemon/binary/install path is needed. Only an
// already-open reclaim FD is inherited; no cgroup path or shell is interpreted.
// A potentially blocking memory.reclaim write can therefore be cancelled by
// killing OUR helper, never by killing a protected workload.
func init() {
	if os.Getenv(reclaimWorkerEnvironment) != "1" || len(os.Args) != 3 || os.Args[1] != reclaimWorkerArgument {
		return
	}
	amount, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil || amount == 0 {
		os.Exit(125)
	}
	value := []byte(strconv.FormatUint(amount, 10))
	n, err := syscall.Write(3, value)
	if err != nil || n != len(value) {
		os.Exit(126)
	}
	os.Exit(0)
}

type linuxKernel struct {
	root   int
	mu     sync.Mutex
	worker <-chan error
}

type rootLock struct {
	kernel *linuxKernel
	once   sync.Once
	err    error
}

func (l *rootLock) Close() error {
	l.once.Do(func() {
		// Closing the only descriptor of this open file description releases
		// its flock. Every other descriptor is a separate O_CLOEXEC openat.
		l.err = syscall.Close(l.kernel.root)
	})
	return l.err
}

func newLinuxKernel(root string) (Kernel, error) {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	var fs syscall.Statfs_t
	if err = syscall.Fstatfs(fd, &fs); err != nil || fs.Type != cgroup2Magic {
		_ = syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("host-memory: unified cgroup-v2 filesystem required")
	}
	return &linuxKernel{root: fd}, nil
}

func (k *linuxKernel) Lock() (io.Closer, error) {
	if err := syscall.Flock(k.root, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = syscall.Close(k.root)
		return nil, fmt.Errorf("host-memory: node cgroup-root lock: %w", err)
	}
	return &rootLock{kernel: k}, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("host-memory: nil I/O context")
	}
	return ctx.Err()
}

func relative(p string) error {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\x00") {
		return errors.New("host-memory: unsafe kernel path")
	}
	return nil
}

// open walks from the locked root with fresh descriptors. openat(".") creates
// a new open file description atomically close-on-exec (no dup/CloseOnExec
// window), so no concurrently forked shim or CNI child can inherit the
// descriptor that carries the node flock.
func (k *linuxKernel) open(p string, flags int) (int, error) {
	parent, err := syscall.Openat(k.root, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil || p == "" {
		return parent, err
	}
	if err = relative(p); err != nil {
		_ = syscall.Close(parent)
		return -1, err
	}
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := syscall.Openat(parent, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(parent)
		if openErr != nil {
			return -1, openErr
		}
		parent = next
	}
	fd, err := syscall.Openat(parent, parts[len(parts)-1], flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	_ = syscall.Close(parent)
	return fd, err
}

// Validate checks the cgroup-v2 root. The kernel never creates cgroup.type on
// the root cgroup (CFTYPE_NOT_ON_ROOT); the root is a domain by definition.
// Non-root domain checks happen where the guard enables controllers.
func (k *linuxKernel) Validate(ctx context.Context) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	var fs syscall.Statfs_t
	if err := syscall.Fstatfs(k.root, &fs); err != nil {
		return err
	}
	if fs.Type != cgroup2Magic {
		return errors.New("host-memory: cgroup filesystem changed")
	}
	controllers, err := k.Read(ctx, "cgroup.controllers")
	if err != nil {
		return err
	}
	enabled, err := k.Read(ctx, "cgroup.subtree_control")
	if err != nil {
		return err
	}
	if !word(controllers, "memory") || !word(enabled, "memory") {
		return errors.New("host-memory: root memory controller must be delegated by node configuration")
	}
	return nil
}

func (k *linuxKernel) Identity(ctx context.Context, p string) (string, error) {
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	fd, err := k.open(p, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if err = syscall.Fstat(fd, &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), ctx.Err()
}

func (k *linuxKernel) Exists(ctx context.Context, p string) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	fd, err := k.open(p, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = syscall.Close(fd)
	return true, nil
}

// Mkdir creates only the aggregate and runtime-private roots. A kubelet
// original or any other host cgroup is never created by the guard.
func (k *linuxKernel) Mkdir(ctx context.Context, p string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := relative(p); err != nil {
		return err
	}
	parent, base := path.Dir(p), path.Base(p)
	owned := p == aggregate ||
		parent == aggregate && ownedName(base, bestEffortPrefix) ||
		parent != "." && !strings.HasPrefix(parent, aggregate+"/") && parent != aggregate && ownedName(base, protectedPrefix) && !ownedPrivate(parent)
	if !owned {
		return errors.New("host-memory: creation outside owned cgroups refused")
	}
	if parent == "." {
		parent = ""
	}
	fd, err := k.open(parent, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err = syscall.Mkdirat(fd, base, 0755); err != nil {
		return err
	}
	return ctx.Err()
}

func (k *linuxKernel) Children(ctx context.Context, p string) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	fd, err := k.open(p, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), p)
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("host-memory: symlink in cgroup hierarchy")
		}
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, ctx.Err()
}

func readFD(fd int) (string, error) {
	var buffer [4096]byte
	n, err := syscall.Pread(fd, buffer[:], 0)
	if err != nil {
		return "", err
	}
	if n == len(buffer) {
		return "", errors.New("host-memory: oversized kernel control file")
	}
	return string(buffer[:n]), nil
}

func (k *linuxKernel) Read(ctx context.Context, p string) (string, error) {
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	fd, err := k.open(p, syscall.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)
	value, err := readFD(fd)
	if err != nil {
		return "", err
	}
	return value, ctx.Err()
}

func (k *linuxKernel) Write(ctx context.Context, p, value string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	// memory.reclaim must NEVER use this in-process path: its implementation
	// can perform reclaim even with O_NONBLOCK. Reclaim uses the helper below.
	switch path.Base(p) {
	case "memory.high", "memory.max", "memory.swap.max", "memory.oom.group", "cgroup.subtree_control", "cgroup.kill":
	default:
		return errors.New("host-memory: unsupported synchronous kernel write")
	}
	if path.Base(p) == "cgroup.kill" && p != aggregate+"/cgroup.kill" {
		return errors.New("host-memory: kill outside owned aggregate refused")
	}
	fd, err := k.open(p, syscall.O_WRONLY|syscall.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	n, err := syscall.Write(fd, []byte(value))
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	return ctx.Err()
}

func (k *linuxKernel) Reclaim(ctx context.Context, p string, amount uint64) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if p != aggregate || amount == 0 {
		return errors.New("host-memory: reclaim outside owned aggregate refused")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.worker != nil {
		select {
		case <-k.worker:
			k.worker = nil
		default:
			return errors.New("host-memory: previous cancelled reclaim helper has not exited")
		}
	}
	fd, err := k.open(p+"/memory.reclaim", syscall.O_WRONLY|syscall.O_NONBLOCK)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "memory.reclaim")
	defer file.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, reclaimWorkerArgument, strconv.FormatUint(amount, 10))
	cmd.Env = []string{reclaimWorkerEnvironment + "=1"}
	cmd.ExtraFiles = []*os.File{file}
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	k.worker = done
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		k.worker = nil
		return err
	case <-ctx.Done():
		// Do NOT wait synchronously after SIGKILL: kernel reclaim may be stalled.
		// Retain the pending helper and refuse another until it has really exited.
		_ = cmd.Process.Kill()
		return ctx.Err()
	}
}

func (k *linuxKernel) WaitUnpopulated(ctx context.Context, p string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	events, err := k.open(p+"/cgroup.events", syscall.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer syscall.Close(events)
	epoll, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(epoll)
	if err = syscall.EpollCtl(epoll, syscall.EPOLL_CTL_ADD, events, &syscall.EpollEvent{Events: syscall.EPOLLPRI | syscall.EPOLLERR | syscall.EPOLLHUP, Fd: int32(events)}); err != nil {
		return err
	}
	var wake [2]int
	if err = syscall.Pipe2(wake[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		return err
	}
	defer syscall.Close(wake[0])
	defer syscall.Close(wake[1])
	if err = syscall.EpollCtl(epoll, syscall.EPOLL_CTL_ADD, wake[0], &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(wake[0])}); err != nil {
		return err
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_, _ = syscall.Write(wake[1], []byte{1})
		case <-stop:
		}
	}()
	defer func() { close(stop); <-stopped }()
	observed := make([]syscall.EpollEvent, 2)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		text, readErr := readFD(events)
		if readErr != nil {
			return readErr
		}
		populated, parseErr := population(text)
		if parseErr != nil {
			return parseErr
		}
		if !populated {
			return nil
		}
		// Subscribe-before-read avoids a missed final-population transition. The
		// kernel, not a poll/sleep loop, wakes this wait. Cancellation uses a pipe.
		_, err = syscall.EpollWait(epoll, observed, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

// Remove rmdirs only a runtime-private cgroup or its descendant. The kernel
// refuses a cgroup with tasks or live children; residual charges reparent.
func (k *linuxKernel) Remove(ctx context.Context, p string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := relative(p); err != nil {
		return err
	}
	if !ownedPrivate(p) {
		return errors.New("host-memory: removal outside owned private cgroup refused")
	}
	fd, err := k.open(path.Dir(p), syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	name, err := syscall.BytePtrFromString(path.Base(p))
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(fd), uintptr(unsafe.Pointer(name)), 0x200 /* AT_REMOVEDIR */)
	if errno != 0 {
		return errno
	}
	return ctx.Err()
}
