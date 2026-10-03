//go:build windows

package jobs

import "golang.org/x/sys/windows"

func lockFile(path string) (func() error, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		return nil, e
	}
	return func() error { return windows.CloseHandle(h) }, nil
}
func syncDirectory(string) {}
