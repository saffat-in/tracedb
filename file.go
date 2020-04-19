package tracedb

import (
	"encoding"
	"os"

	"github.com/unit-io/tracedb/fs"
)

type file struct {
	fs.FileManager

	size int64
}

func newFile(fs fs.FileSystem, name string) (file, error) {
	fileFlag := os.O_CREATE | os.O_RDWR
	fileMode := os.FileMode(0666)
	fi, err := fs.OpenFile(name, fileFlag, fileMode)
	f := file{}
	if err != nil {
		return f, err
	}
	f.FileManager = fi
	stat, err := fi.Stat()
	if err != nil {
		return f, err
	}
	f.size = stat.Size()
	return f, err
}

func (f *file) truncate(size int64) error {
	if err := f.Truncate(size); err != nil {
		return err
	}
	f.size = size
	return nil
}

func (f *file) extend(size uint32) (int64, error) {
	off := f.size
	if err := f.Truncate(off + int64(size)); err != nil {
		return 0, err
	}
	f.size += int64(size)

	return off, nil
}

func (f *file) write(data []byte) (int, error) {
	off := f.size
	if _, err := f.WriteAt(data, off); err != nil {
		return 0, err
	}
	f.size += int64(len(data))
	return len(data), nil
}

func (f *file) writeMarshalableAt(m encoding.BinaryMarshaler, off int64) error {
	buf, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = f.WriteAt(buf, off)
	return err
}

func (f *file) readUnmarshalableAt(m encoding.BinaryUnmarshaler, size uint32, off int64) error {
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	return m.UnmarshalBinary(buf)
}

func (f *file) currSize() int64 {
	return f.size
}

func (f *file) Size() int64 {
	stat, _ := f.Stat()
	return stat.Size()
}
