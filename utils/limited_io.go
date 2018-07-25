// Copyright 2018 Northern.tech AS
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

type LimitedBufferedWriter interface {
	Write(buf []byte) (int, error)
	Flush() (int, error)
}

func NewLimitedReadSeeker(file io.ReadSeeker, sizeCap uint64) io.ReadSeeker {
	return &limitedIO{R: file, S: file, N: sizeCap, s: 0}
}

func NewLimitedReader(file io.Reader, sizeCap uint64) io.Reader {
	return &limitedIO{R: file, N: sizeCap, s: 0}
}

func NewLimitedWriter(file io.Writer, sizeCap uint64) io.Writer {
	return &limitedIO{W: file, N: sizeCap, s: 0}
}

func NewLimitedBufferedWriter(file io.Writer, sizeCap uint64, buf_sz int) LimitedBufferedWriter {
	return &limitedBufferedWriter{W: file, N: sizeCap,
		buf: make([]byte, buf_sz), buf_sz: buf_sz, buf_n: 0}
}

// General io structure.
// For the interfaces above only a subset is actually in use.
type limitedIO struct {
	W io.Writer // underlying writer
	R io.Reader // underlying reader
	S io.Seeker // underlying seeker
	N uint64    // number of bytes in total
	s uint64    // seek set
}

type limitedBufferedWriter struct {
	W      io.Writer // underlying writer
	N      uint64    // Number of bytes in total
	s      uint64    // seek set
	buf    []byte    // internal buffer
	buf_sz int       // buffer size
	buf_n  int       // bytes in buffer
}

func (lio *limitedIO) Write(p []byte) (int, error) {
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
	lio.s += uint64(w)
	if err != nil {
		selferr = err
	}
	return w, selferr
}

func (lio *limitedIO) Read(p []byte) (int, error) {
	if lio.R == nil {
		return 0, syscall.EBADF
	}
	var bytesRead int
	var err error
	toRead := len(p)

	if uint64(toRead) > lio.N {
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

func (lio *limitedIO) Seek(offset int64, whence int) (int64, error) {
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

// Writes in multiples of buffer size, and bufferes the rest.
func (lbw *limitedBufferedWriter) Write(p []byte) (int, error) {
	if lbw.W == nil {
		return 0, syscall.EBADF
	}
	var selferr error
	var toWrite []byte
	toBuffer := (len(p) + lbw.buf_n) % lbw.buf_sz

	// EOF reached?
	if (uint64(len(p)+lbw.buf_n) + lbw.s) > lbw.N {
		selferr = syscall.ENOSPC
		// https://godoc.org/io#Writer Write writes len(p) bytes from p to the
		// underlying data stream. It returns the number of bytes written from p (0
		// <= n <= len(p)) and any error encountered that caused the write to stop
		// early.
		if uint64(lbw.buf_n)+lbw.s > lbw.N {
			w, _ := lbw.Flush()
			return w, selferr
		} else {
			toWrite = append(lbw.buf[:lbw.buf_n], p[:(lbw.N-lbw.s-uint64(lbw.buf_n))]...)
			w, err := lbw.W.Write(toWrite)
			if err != nil {
				selferr = err
			}
			return w, selferr
		}
	}

	toWrite = append(lbw.buf[:lbw.buf_n], p[:(len(p)-toBuffer)]...)
	w, err := lbw.W.Write(toWrite)
	lbw.s += uint64(w)

	if err != nil {
		selferr = err
	}

	lbw.buf_n = toBuffer
	copy(lbw.buf[:lbw.buf_n], p[(len(p)-lbw.buf_n):])

	return w, selferr
}

// Flushes buffer by writing to file.
func (lbw *limitedBufferedWriter) Flush() (int, error) {
	var err error
	var w int

	if uint64(lbw.buf_n)+lbw.s > lbw.N {
		w, err = lbw.W.Write(lbw.buf[:lbw.N-lbw.s])
		if err == nil {
			err = syscall.ENOSPC
		}
	} else {
		w, err = lbw.W.Write(lbw.buf[:lbw.buf_n])
	}

	lbw.buf_n -= w
	return w, err
}
