package mai

import (
	"errors"
	"io"
	"os"
)

var (
	errNotRegularFile = errors.New("not a regular file")
	errFileTooLarge   = errors.New("file exceeds size limit")
)

// readBoundedFile leaves opening policy and closing ownership with the caller.
func readBoundedFile(file *os.File, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	if info.Size() > limit {
		return nil, errFileTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errFileTooLarge
	}

	return data, nil
}
