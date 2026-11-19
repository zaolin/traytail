package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

// instanceLock prevents duplicate tray icons and double logging when
// both a service and a manual invocation are started. Uses flock so
// the lock is released automatically when the owning process exits.
type instanceLock struct {
	file *os.File
}

func (l *instanceLock) Close() {
	if l != nil && l.file != nil {
		_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		_ = l.file.Close()
	}
}

// errInstanceRunning marks the expected duplicate-instance outcome.
var errInstanceRunning = errors.New("traytail is already running")

// processLock retains the lock for the process lifetime; without this
// reference the GC could finalize and close the file while traytail
// is still running, allowing a duplicate instance.
var processLock *instanceLock

// singleInstancePath is a seam for tests; production locks the
// per-user temp path.
var singleInstancePath = func() string {
	return fmt.Sprintf("%s/traytail-%d.lock", os.TempDir(), os.Getuid())
}

// singleInstance acquires the per-user instance lock. Contention
// returns errInstanceRunning; other errors are returned unwrapped so
// callers can distinguish configuration/permission problems.
func singleInstance() (*instanceLock, error) {
	path := singleInstancePath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errInstanceRunning
		}
		return nil, fmt.Errorf("lock traytail: %w", err)
	}
	return &instanceLock{file: f}, nil
}

// mustSingleInstance is the production helper. A duplicate exits
// quietly (not an error); a genuine lock/setup error is fatal so
// systemd's Restart=on-failure can retry.
func mustSingleInstance() {
	lock, err := singleInstance()
	if err != nil {
		if errors.Is(err, errInstanceRunning) {
			log.Printf("traytail: %v", err)
			os.Exit(0)
		}
		log.Fatalf("traytail: cannot create instance lock: %v", err)
	}
	processLock = lock
}
