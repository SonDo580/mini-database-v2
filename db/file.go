package db

import (
	"fmt"
	"os"
	"path"
	"syscall"
)

// open or create a file; fsync the directory
func createFileSync(file string) (fd int, err error) {
	// obtain the directory fd
	flags := os.O_RDONLY | syscall.O_DIRECTORY
	dirfd, err := syscall.Open(path.Dir(file), flags, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open directory: %w", err)
	}
	defer syscall.Close(dirfd)

	// open or create the file
	// . use openat() to guaranteed the file is from the same directory we opened,
	//   in case the directory path is replaced in between (race condition)
	flags = os.O_RDWR | os.O_CREATE
	fd, err = syscall.Openat(dirfd, path.Base(file), flags, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open file: %w", err)
	}

	// fsync the directory
	if err = syscall.Fsync(dirfd); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("fsync directory: %w", err)
	}

	return fd, nil
}
