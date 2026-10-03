//go:build !windows

package jobs

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(path string) (func() error, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return f.Close, nil
}
func syncDirectory(path string) {
	if f, e := os.Open(path); e == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}
