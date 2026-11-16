package fileops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Resolve resolves the opened target so junctions are included in containment
// checks.
func Resolve(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer windows.CloseHandle(handle)
	var short [windows.MAX_PATH]uint16
	buffer := short[:]
	// Zero requests a normalized path with a DOS drive letter or UNC share.
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err == nil && n >= uint32(len(buffer)) {
		buffer = make([]uint16, 32768) // Maximum Windows extended path length.
		n, err = windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	}
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	if n >= uint32(len(buffer)) {
		return "", fmt.Errorf("resolved path exceeds Windows path length")
	}
	resolved := windows.UTF16ToString(buffer[:n])
	if tail, unc := strings.CutPrefix(resolved, `\\?\UNC\`); unc {
		return `\\` + tail, nil
	}
	return strings.TrimPrefix(resolved, `\\?\`), nil
}

func windowsAPIPath(path string) string {
	if len(path) < 248 || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if tail, unc := strings.CutPrefix(path, `\\`); unc {
		return `\\?\UNC\` + tail
	}
	return `\\?\` + path
}

// OpenDirectory opens a directory for bounded staging scans without preventing
// engine scratch cleanup or rename.
func OpenDirectory(path string) (*os.File, error) {
	wide, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return nil, err
	}
	// Share deletion so scanning does not interfere with engine scratch cleanup.
	handle, err := windows.CreateFile(wide, windows.FILE_GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// LinkCount returns the number of hard-linked names for an opened file.
func LinkCount(file *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

// Publish moves source to destination exclusively, preserving an existing path.
func Publish(source, destination string) error {
	src, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	dst, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// MoveFile provides no-replace semantics, including FAT/exFAT destinations.
	return syscall.MoveFile(src, dst)
}
