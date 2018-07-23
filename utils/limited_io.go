// Copyright 2017 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package utils

import (
	"io"
	"os"
	"syscall"
)

type LimitedIO struct {
	W io.Writer // underlying writer
	R io.Reader // underlying reader
	S io.Seeker // underlying seeker
	N uint64    // number of bytes in total
	s uint64    // seek set
}

func NewLimitedIO(file *os.File, size uint64) *LimitedIO {
	return &LimitedIO{W: file, R: file, S: file, N: size, s: 0}
}

func (lio *LimitedIO) Write(p []byte) (int, error) {
	if lio.W == nil {
		return 0, syscall.EBADF
	}
	var selferr error
	toWrite := p

	if (uint64(len(p)) + lio.s) > lio.N {
		// https://godoc.org/io#Writer Write writes len(p) bytes from p to the
		// underlying data stream. It returns the number of bytes written from p (0
		// <= n <= len(p)) and any error encountered that caused the write to stop
		// early.
		toWrite = p[:(lio.N - lio.s)]
		selferr = syscall.ENOSPC
	}

	w, err := lio.W.Write(toWrite)
	if w != 0 {
		lio.s += uint64(w)
	}
	if err != nil {
		selferr = err
	}
	return w, selferr
}

func (lio *LimitedIO) Read(p []byte) (int, error) {
	if lio.R == nil {
		return 0, syscall.EBADF
	}
	var bytesRead int
	var err error
	toRead := len(p)

	if uint64(toRead) > lio.N {
		// https://godoc.org/io#Writer Write writes len(p) bytes from p to the
		// underlying data stream. It returns the number of bytes written from p (0
		// <= n <= len(p)) and any error encountered that caused the write to stop
		// early.
		toRead = int(lio.s)
		r := io.LimitReader(lio.R, int64(toRead))
		bytesRead, err = r.Read(p)
	} else {
		bytesRead, err = lio.R.Read(p)
	}

	if bytesRead != 0 {
		lio.s += uint64(bytesRead)
	}
	return bytesRead, err
}

func (lio *LimitedIO) Seek(offset int64, whence int) (int64, error) {
	if offset > int64(lio.N) {
		// if we seek beyond the partition, we catch this by setting
		// the seek to the end of the partition, and leave Read/write to
		// handle subsequent I/O.
		if whence == os.SEEK_SET || whence == os.SEEK_CUR {
			return lio.S.Seek(int64(lio.N), whence)
		}
	}
	return lio.S.Seek(offset, whence)
}
