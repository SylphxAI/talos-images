package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// nativePressureTrigger is the earliest memory stall event Linux supports for
// a privileged PSI trigger: any stall within the minimum 500ms window. It is an
// event source for re-reading the live budget, not a capacity margin.
// https://docs.kernel.org/accounting/psi.html#monitoring-for-pressure-thresholds
const nativePressureTrigger = "some 1 500000"

// nativePressureWatcher re-reconciles Guard on kernel pressure events. It
// blocks in poll(2); there is no timer or polling interval. It lives inside the
// containerd process and stops with the CRI service.
type nativePressureWatcher struct {
	wake      *os.File // write end; one byte wakes poll(2) for shutdown
	wait      *os.File // read end, polled by run
	done      chan struct{}
	cancel    context.CancelFunc
	timeout   time.Duration
	mu        sync.Mutex
	failure   error
	onFailure func(error)
}

func startNativePressureWatcher(path string, reconcile func(context.Context) error, timeout time.Duration, failure ...func(error)) (*nativePressureWatcher, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// The kernel parses the NUL-terminated trigger as documented.
	if _, err := unix.Write(fd, append([]byte(nativePressureTrigger), 0)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("install PSI trigger: %w", err)
	}
	return watchNativePressure(fd, reconcile, timeout, failure...)
}

// watchNativePressure owns fd and invokes reconcile on POLLPRI.
func watchNativePressure(fd int, reconcile func(context.Context) error, timeout time.Duration, failure ...func(error)) (*nativePressureWatcher, error) {
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		unix.Close(fd)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &nativePressureWatcher{wake: os.NewFile(uintptr(pipe[1]), "native-pressure-wake"), wait: os.NewFile(uintptr(pipe[0]), "native-pressure-wait"),
		done: make(chan struct{}), cancel: cancel, timeout: timeout}
	if len(failure) > 0 {
		w.onFailure = failure[0]
	}
	go w.run(ctx, fd, pipe[0], reconcile)
	return w, nil
}

func (w *nativePressureWatcher) run(ctx context.Context, fd, stop int, reconcile func(context.Context) error) {
	defer close(w.done)
	defer unix.Close(fd)
	// The wake pipe stays open until Close has observed done.
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI}, {Fd: int32(stop), Events: unix.POLLIN}}
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			w.fail(err)
			log.L.WithError(err).Error("native host memory pressure watcher stopped")
			return
		}
		if fds[1].Revents != 0 || ctx.Err() != nil {
			return
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			w.fail(errors.New("native host memory pressure monitor was destroyed"))
			log.L.Error("native host memory pressure monitor was destroyed")
			return
		}
		if fds[0].Revents&unix.POLLPRI != 0 {
			bounded, cancel := context.WithTimeout(ctx, w.timeout)
			if err := reconcile(bounded); err != nil && ctx.Err() == nil {
				w.fail(err)
				log.L.WithError(err).Error("native host memory pressure reconcile failed; BE disabled")
				cancel()
				return
			}
			cancel()
		}
	}
}

// Close cancels an in-flight reconcile and waits at most the reconcile bound.
// The wake pipe is closed only after the watcher goroutine has exited.
func (w *nativePressureWatcher) Close() error {
	w.cancel()
	_, err := w.wake.Write([]byte{1})
	select {
	case <-w.done:
	case <-time.After(w.timeout):
		return errors.Join(err, errors.New("native host memory pressure watcher did not stop"))
	}
	return errors.Join(err, w.wake.Close(), w.wait.Close())
}

// Err checks the installed kernel watcher, not a configuration promise. Failure
// is latched: a successful unrelated allocation cannot clear a dead observer.
func (w *nativePressureWatcher) Err() error {
	w.mu.Lock()
	err := w.failure
	w.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-w.done:
		return errors.New("native pressure observer stopped")
	default:
		return nil
	}
}
func (w *nativePressureWatcher) fail(err error) {
	w.mu.Lock()
	w.failure = err
	w.mu.Unlock()
	if w.onFailure != nil {
		w.onFailure(err)
	}
}
