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
	"io/ioutil"
	"os"
	"path"
	"syscall"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func TestBlockDeviceFail(t *testing.T) {
	bd := blockDevice{Path: "/dev/somefile"}

	// closing unopened device should not fail
	err := bd.Close()
	assert.NoError(t, err)

	w, err := bd.Write([]byte("foo"))
	assert.Equal(t, 0, w)
	assert.Error(t, err)

	err = bd.Close()
	assert.NoError(t, err)
}

func makeBlockDeviceSize(t *testing.T, sz uint64, err error, name string) BlockDeviceGetSizeFunc {
	return func(file *os.File) (uint64, error) {
		t.Logf("block device size called: %v", file)
		if assert.NotNil(t, file) {
			assert.Equal(t, name, file.Name())
		}
		return sz, err
	}
}

func makeBlockDeviceSectorSize(t *testing.T, ssz int, err error, name string) BlockDeviceGetSectorSizeFunc {
	return func(file *os.File) (int, error) {
		t.Logf("block device sector size called: %v", file)
		if assert.NotNil(t, file) {
			assert.Equal(t, name, file.Name())
		}
		return ssz, err
	}
}

func createFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}

func TestBlockDeviceWrite(t *testing.T) {
	td, err := ioutil.TempDir("", "mender-block-device-")
	assert.NoError(t, err)
	defer os.RemoveAll(td)

	// prepare fake block device file
	bdpath := path.Join(td, "foo")

	// temporarily override helper for getting block device size
	old := BlockDeviceGetSizeOf

	// pretend the device is only 10 bytes in size
	BlockDeviceGetSizeOf = makeBlockDeviceSize(t, 10, nil, bdpath)
	BlockDeviceGetSectorSizeOf = makeBlockDeviceSectorSize(t, 10, nil, bdpath)

	// test simple write
	err = createFile(bdpath)
	assert.NoError(t, err)
	bd := blockDevice{Path: bdpath}
	n, err := bd.Write([]byte("foobar"))
	assert.Equal(t, 6, n)
	assert.NoError(t, err)
	err = bd.Close() // also fsyncs
	assert.NoError(t, err, syscall.ENOSPC.Error())

	data, err := ioutil.ReadFile(bdpath)
	assert.NoError(t, err)
	assert.Equal(t, []byte("foobar"), data)

	os.Remove(bdpath)

	// too large write
	err = createFile(bdpath)
	assert.NoError(t, err)
	bd = blockDevice{Path: bdpath}
	n, err = bd.Write([]byte("foobarfoobar"))
	assert.Equal(t, 10, n)
	assert.EqualError(t, err, syscall.ENOSPC.Error())
	err = bd.Close()

	data, err = ioutil.ReadFile(bdpath)
	assert.NoError(t, err)
	// written only 10 bytes
	assert.Equal(t, []byte("foobarfoob"), data)

	os.Remove(bdpath)

	BlockDeviceGetSizeOf = old
}

func TestBlockDeviceSize(t *testing.T) {
	td, err := ioutil.TempDir("", "mender-block-device-")
	assert.NoError(t, err)
	defer os.RemoveAll(td)

	// prepare fake block device file
	bdpath := path.Join(td, "foo")
	err = createFile(bdpath)
	assert.NoError(t, err)

	// temporarily override helper for getting block device size
	old := BlockDeviceGetSizeOf

	// pretend the device is only 10 bytes in size
	BlockDeviceGetSizeOf = makeBlockDeviceSize(t, 10, nil, bdpath)
	BlockDeviceGetSectorSizeOf = makeBlockDeviceSectorSize(t, 10, nil, bdpath)

	bd := blockDevice{Path: bdpath}
	sz, err := bd.Size()
	assert.Equal(t, uint64(10), sz)
	assert.NoError(t, err)

	BlockDeviceGetSizeOf = makeBlockDeviceSize(t, 10, errors.New("failed"), bdpath)
	BlockDeviceGetSectorSizeOf = makeBlockDeviceSectorSize(t, 10, errors.New("failed"), bdpath)

	bd = blockDevice{Path: bdpath}
	sz, err = bd.Size()
	assert.EqualError(t, err, "failed")

	BlockDeviceGetSizeOf = old
}
