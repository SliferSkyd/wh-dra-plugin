package flock

import (
	"context"
	"os"
	"syscall"
	"time"
)

// Flock is a file-based exclusive lock. Safe across processes — auto-releases
// if the holder crashes (file descriptor is closed by the OS).
type Flock struct {
	path string
}

func NewFlock(path string) *Flock {
	return &Flock{path: path}
}

// Acquire spins until the exclusive lock is obtained or ctx is cancelled.
// Returns a release func; caller must defer it.
func (f *Flock) Acquire(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { file.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
