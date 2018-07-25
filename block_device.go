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
package main

import (
	"io"
	"os"

	"github.com/mendersoftware/log"
	"github.com/mendersoftware/mender/utils"
)

var (
	BlockDeviceGetSizeOf       BlockDeviceGetSizeFunc       = getBlockDeviceSize
	BlockDeviceGetSectorSizeOf BlockDeviceGetSectorSizeFunc = getBlockDeviceSectorSize
)

type BlockDevice interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
	Size() (uint64, error)
	SectorSize() (int, error)
    GetPath() string
}

// BlockDeviceGetSizeFunc is a helper for obtaining the size of a block device.
type BlockDeviceGetSizeFunc func(file *os.File) (uint64, error)

// BlockDeviceGetSectorSizeFunc is a helper for obtaining the sector size of a block device.
type BlockDeviceGetSectorSizeFunc func(file *os.File) (int, error)

// blockDevice is a low-level wrapper for a block device. The wrapper implements
// io.Writer and io.Closer interfaces.
type blockDevice struct {
	// device path, ex. /dev/mmcblk0p1
	Path string
	// os.File for writing
	out *os.File
	// wrapper for `out` limit the number of bytes written
	w utils.LimitedBufferedWriter
	// wrapper for `out` to limit the size scope of read / seek
	r io.ReadSeeker
	// Set to true if we are updating an UBI volume
	typeUBI bool
	// image size
	ImageSize int64
}

func NewBlockDevice(path string, typeUBI bool, size int64) BlockDevice {
	return &blockDevice{Path: path, typeUBI: typeUBI, ImageSize: size}
}

// Write writes data `p` to underlying block device. Will automatically open
// the device in a write mode. Otherwise, behaves like io.Writer.
func (bd *blockDevice) Write(p []byte) (int, error) {
	if bd.out == nil {
		log.Infof("opening device %s for writing", bd.Path)
		out, err := os.OpenFile(bd.Path, os.O_WRONLY, 0)
		if err != nil {
			return 0, err
		}

		// From <mtd/ubi-user.h>
		//
		// UBI volume update
		// ~~~~~~~~~~~~~~~~~
		//
		// Volume update should be done via the UBI_IOCVOLUP ioctl command of the
		// corresponding UBI volume character device. A pointer to a 64-bit update
		// size should be passed to the ioctl. After this, UBI expects user to write
		// this number of bytes to the volume character device. The update is finished
		// when the claimed number of bytes is passed. So, the volume update sequence
		// is something like:
		//
		// fd = open("/dev/my_volume");
		// ioctl(fd, UBI_IOCVOLUP, &image_size);
		// write(fd, buf, image_size);
		// close(fd);
		if bd.typeUBI {
			err := setUbiUpdateVolume(out, bd.ImageSize)
			if err != nil {
				log.Errorf("Failed to write images size to UBI_IOCVOLUP: %v", err)
				return 0, err
			}
		}

		size, err := BlockDeviceGetSizeOf(out)
		if err != nil {
			log.Errorf("failed to read block device size: %v", err)
			out.Close()
			return 0, err
		}
		log.Infof("partition %s size: %v", bd.Path, size)

		sectSize, err := BlockDeviceGetSectorSizeOf(out)
		if err != nil {
			log.Errorf("failed to read block device sector size: %v", err)
			out.Close()
			return 0, err
		}
		log.Infof("partition %s sector size: %v", bd.Path, size)
		bd.out = out
		bd.w = utils.NewLimitedBufferedWriter(out, size, sectSize)
	}

	w, err := bd.w.Write(p)
	if err != nil {
		log.Errorf("written %v out of %v bytes to partition %s: %v",
			w, len(p), bd.Path, err)
	}
	return w, err
}

// Reads data to `p` from underlying block device. Will automatically open
// the device in a read-only mode. Otherwise, behaves like io.Reader.
// NOTE: once Read is called, one can't call write untill the block device
//       is closed.
func (bd *blockDevice) Read(p []byte) (int, error) {
	if bd.out == nil {
		log.Infof("opening device %s for reading", bd.Path)
		out, err := os.OpenFile(bd.Path, os.O_RDONLY, 0)
		if err != nil {
			return 0, err
		}

		size, err := BlockDeviceGetSizeOf(out)
		if err != nil {
			log.Errorf("failed to read block device size: %v", err)
			out.Close()
			return 0, err
		}
		log.Infof("partition %s size: %v", bd.Path, size)

		bd.out = out
		bd.r = utils.NewLimitedReadSeeker(out, size)
	}

	r, err := bd.r.Read(p)
	if err != nil {
		log.Errorf("read %v out of %v bytes from partition %s: %v",
			r, len(p), bd.Path, err)
	}
	return r, err
}

// Seek should only be used if the device is opened for reading.
func (bd *blockDevice) Seek(offset int64, whence int) (int64, error) {
	return bd.r.Seek(offset, whence)
}

// Close closes underlying block device automatically syncing any unwritten
// data. Othewise, behaves like io.Closer.
func (bd *blockDevice) Close() error {
	var selferr error
	if bd.out != nil {
		if _, selferr := bd.w.Flush(); selferr != nil {
			log.Errorf("error flushing buffer to partition %s: %v",
				bd.Path, selferr)
		}
		if err := bd.out.Sync(); err != nil {
			log.Errorf("failed to fsync partition %s: %v", bd.Path, err)
			return err
		}
		if err := bd.out.Close(); err != nil {
			log.Errorf("failed to close partition %s: %v", bd.Path, err)
			selferr = err
		}
		bd.out = nil
		bd.w = nil
	}

	return selferr
}

// Size queries the size of the underlying block device. Automatically opens a
// new fd in O_RDONLY mode, thus can be used in parallel to other operations.
func (bd *blockDevice) Size() (uint64, error) {
	out, err := os.OpenFile(bd.Path, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	return BlockDeviceGetSizeOf(out)
}

// SectorSize queries the logical sector size of the underlying block device. Automatically opens a
// new fd in O_RDONLY mode, thus can be used in parallel to other operations.
func (bd *blockDevice) SectorSize() (int, error) {
	out, err := os.OpenFile(bd.Path, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	return BlockDeviceGetSectorSizeOf(out)
}

func (bd *blockDevice) GetPath() string {return bd.Path}
