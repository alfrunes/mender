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
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/alfrunes/xdelta"
	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
)

type deviceConfig struct {
	rootfsPartA string
	rootfsPartB string
}

type device struct {
	BootEnvReadWriter
	Commander
	*partitions
}

var (
	errorNoUpgradeMounted = errors.New("There is nothing to commit")
)

func NewDevice(env BootEnvReadWriter, sc StatCommander, config deviceConfig) *device {
	partitions := partitions{
		StatCommander:     sc,
		BootEnvReadWriter: env,
		rootfsPartA:       config.rootfsPartA,
		rootfsPartB:       config.rootfsPartB,
		active:            "",
		inactive:          "",
	}
	device := device{env, sc, &partitions}
	return &device
}

func (d *device) Reboot() error {
	log.Info("Mender rebooting from active partition: %s", d.active)
	return d.Command("reboot").Run()
}

func (d *device) SwapPartitions() error {
	// first get inactive partition
	inactivePartition, inactivePartitionHex, err := d.getInactivePartition()
	if err != nil {
		return err
	}
	log.Infof("setting partition for rollback: %s", inactivePartition)

	err = d.WriteEnv(BootVars{"mender_boot_part": inactivePartition, "mender_boot_part_hex": inactivePartitionHex, "upgrade_available": "0"})
	if err != nil {
		return err
	}
	log.Debug("Marking inactive partition as a boot candidate successful.")
	return nil
}

// setupInstall prepares inactive partition for installation
func (d *device) setupInstall(size int64) (BlockDevice, error) {

	inactivePartition, err := d.GetInactive()
	if err != nil {
		return nil, err
	}

	typeUBI := isUbiBlockDevice(inactivePartition)
	if typeUBI {
		// UBI block devices are not prefixed with /dev due to the fact
		// that the kernel root= argument does not handle UBI block
		// devices which are prefixed with /dev
		//
		// Kernel root= only accepts:
		// - ubi0_0
		// - ubi:rootfsa
		inactivePartition = filepath.Join("/dev", inactivePartition)
	}

	b := NewBlockDevice(inactivePartition, typeUBI, size)

	if bsz, err := b.Size(); err != nil {
		log.Errorf("failed to read size of block device %s: %v",
			inactivePartition, err)
		return nil, err
	} else if bsz < uint64(size) {
		log.Errorf("update (%v bytes) is larger than the size of device %s (%v bytes)",
			size, inactivePartition, bsz)
		return nil, syscall.ENOSPC
	}
	return b, nil
}

// DeltaUpdate opens active and inactive partitions and returns them as reader
// and writer respectively along with an allocated buffer for use in the xdelta wrapper
// and an error if any.
// NOTE: size is with respect to the patched rootfs, not the patch itself.
// TODO: make it possible to pass delta flags (such as compression type),
//       and check their validity.
func (d *device) InstallDeltaUpdate(patch io.ReadCloser, size int64) error {
	log.Debugf("Preparing to install delta update of size: %d", size)
	if size < 0 {
		return errors.New("Invalid delta update. Aborting.")
	}

	bd_inactive, err := d.setupInstall(size)
	if err != nil {
		return err
	}
	defer bd_inactive.Close()

	// setup active partition aswell (RD_ONLY)
	activePartition, err := d.GetActive()
	if err != nil {
		return err
	}
	typeUBI := isUbiBlockDevice(activePartition)
	if typeUBI {
		activePartition = filepath.Join("/dev", activePartition)
	}
	bd_active := NewBlockDevice(activePartition, typeUBI, size)
	defer bd_active.Close()

	// Use inactive partition as source, and decode patch to inactive partition
	coder := xdelta.NewXdeltaCoder(bd_active, xdelta.XD3_ADLER32|xdelta.XD3_SECONDARY_DJW)
	err = coder.Decode(patch, bd_inactive)

	if err != nil {
		log.Errorf("Delta update decoding failed: %v", err)
		return err
	}
	return nil
}

func (d *device) InstallUpdate(image io.ReadCloser, size int64) error {

	log.Debugf("Trying to install update of size: %d", size)
	if image == nil || size < 0 {
		return errors.New("Have invalid update. Aborting.")
	}

	b, err := d.setupInstall(size)
	if err != nil {
		return err
	}

	w, err := io.Copy(b, image)
	if err != nil {
		log.Errorf("failed to write image data to device %v: %v",
			b.GetPath(), err)
	}

	log.Infof("wrote %v/%v bytes of update to device %v",
		w, size, b.GetPath())

	if cerr := b.Close(); cerr != nil {
		log.Errorf("closing device %v failed: %v", b.GetPath(), cerr)
		if err != nil {
			return cerr
		}
	}

	return err
}

func (d *device) getInactivePartition() (string, string, error) {
	inactivePartition, err := d.GetInactive()
	if err != nil {
		return "", "", errors.New("Error obtaining inactive partition: " + err.Error())
	}

	log.Debugf("Marking inactive partition (%s) as the new boot candidate.", inactivePartition)

	partitionNumberDecStr := inactivePartition[len(strings.TrimRight(inactivePartition, "0123456789")):]
	partitionNumberDec, err := strconv.Atoi(partitionNumberDecStr)
	if err != nil {
		return "", "", errors.New("Invalid inactive partition: " + inactivePartition)
	}

	partitionNumberHexStr := fmt.Sprintf("%X", partitionNumberDec)

	return partitionNumberDecStr, partitionNumberHexStr, nil
}

func (d *device) EnableUpdatedPartition() error {

	inactivePartition, inactivePartitionHex, err := d.getInactivePartition()
	if err != nil {
		return err
	}

	log.Info("Enabling partition with new image installed to be a boot candidate: ", string(inactivePartition))
	// For now we are only setting boot variables
	err = d.WriteEnv(BootVars{"upgrade_available": "1", "mender_boot_part": inactivePartition, "mender_boot_part_hex": inactivePartitionHex, "bootcount": "0"})
	if err != nil {
		return err
	}

	log.Debug("Marking inactive partition as a boot candidate successful.")

	return nil
}

func (d *device) CommitUpdate() error {
	// Check if the user has an upgrade to commit, if not, throw an error
	hasUpdate, err := d.HasUpdate()
	if err != nil {
		return err
	}
	if hasUpdate {
		log.Info("Commiting update")
		// For now set only appropriate boot flags
		return d.WriteEnv(BootVars{"upgrade_available": "0"})
	}
	return errorNoUpgradeMounted
}

func (d *device) HasUpdate() (bool, error) {
	env, err := d.ReadEnv("upgrade_available")
	if err != nil {
		return false, errors.Wrapf(err, "failed to read environment variable")
	}
	upgradeAvailable := env["upgrade_available"]

	if upgradeAvailable == "1" {
		return true, nil
	}
	return false, nil
}
