// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2020 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package disks

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/snapcore/snapd/osutil"
)

var (
	luksUUIDPatternRe = regexp.MustCompile(`(?m)CRYPT-LUKS2-([0-9a-f]{32})`)
)

// diskFromMountPoint is exposed for mocking from other tests via
// MockMountPointDisksToPartionMapping, but we can't just assign
// diskFromMountPoint to diskFromMountPointImpl due to signature differences,
// the former returns a Disk, the latter returns a *disk, and as such they can't
// be assigned to each other
var diskFromMountPoint = func(mountpoint string, opts *Options) (Disk, error) {
	return diskFromMountPointImpl(mountpoint, opts)
}

// Options is a set of options used when querying information about
// partition and disk devices.
type Options struct {
	// IsDecryptedDevice indicates that the mountpoint is referring to a
	// decrypted device.
	IsDecryptedDevice bool
}

// Disk is a single physical disk device that contains partitions.
type Disk interface {
	// FindMatchingPartitionUUID finds the partition uuid for a partition
	// matching the specified label on the disk. Note that for non-ascii labels
	// like "Some label", the label should be encoded using \x<hex> for
	// potentially non-safe characters like in "Some\x20Label".
	FindMatchingPartitionUUID(string) (string, error)

	// MountPointIsFromDisk returns whether the specified mountpoint corresponds
	// to a partition on the disk. Note that this only considers partitions
	// and mountpoints found when the disk was identified with
	// DiskFromMountPoint.
	MountPointIsFromDisk(string, *Options) (bool, error)

	// Dev returns the string "major:minor" for the disk device for
	// identification, it should be unique but is not guaranteed to be unique.
	Dev() string
}

type partition struct {
	major    int
	minor    int
	label    string
	partuuid string
	path     string
}

type disk struct {
	major      int
	minor      int
	partitions []*partition
}

func parseDeviceMajorMinor(s string) (int, int, error) {
	errMsg := fmt.Errorf("invalid device number format: (expected <int>:<int>)")
	devNums := strings.SplitN(s, ":", 2)
	if len(devNums) != 2 {
		return 0, 0, errMsg
	}
	maj, err := strconv.Atoi(devNums[0])
	if err != nil {
		return 0, 0, errMsg
	}
	min, err := strconv.Atoi(devNums[1])
	if err != nil {
		return 0, 0, errMsg
	}
	return maj, min, nil
}

var udevadmProperties = func(device string) ([]byte, error) {
	cmd := exec.Command("udevadm", "info", "--query", "property", "--name", device)
	return cmd.CombinedOutput()
}

func udevProperties(device string) (map[string]string, error) {
	out, err := udevadmProperties(device)
	if err != nil {
		return nil, osutil.OutputErr(out, err)
	}
	r := bytes.NewBuffer(out)

	return parseUdevProperties(r)
}

func parseUdevProperties(r io.Reader) (map[string]string, error) {
	m := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		strs := strings.SplitN(scanner.Text(), "=", 2)
		if len(strs) != 2 {
			// bad udev output?
			continue
		}
		m[strs[0]] = strs[1]
	}

	return m, scanner.Err()
}

// DiskFromMountPoint finds a matching Disk for the specified mount point. It
// does a best effort in identifying partitions for the disk, but may be racy
// due to races inherent in the initramfs surrounding udev and sysfs.
func DiskFromMountPoint(mountpoint string, opts *Options) (Disk, error) {
	// call the unexported version that may be mocked by tests
	return diskFromMountPoint(mountpoint, opts)
}

// diskFromMountPointImpl uses the mount table, sysfs and udev to try and
// identify the disk and full set of partitions for the specified mount point.
// since during the initramfs things may be racy around udev adding new devices
// this is a best effort search and partially matching partitions which do not
// have the full information are discarded from the returned disk.
func diskFromMountPointImpl(mountpoint string, opts *Options) (*disk, error) {
	// first get the mount entry for the mountpoint
	mounts, err := osutil.LoadMountInfo()
	if err != nil {
		return nil, err
	}
	found := false
	d := &disk{}
	mountpointPart := partition{}
	for _, mount := range mounts {
		if mount.MountDir == mountpoint {
			mountpointPart.major = mount.DevMajor
			mountpointPart.minor = mount.DevMinor
			mountpointPart.path = mount.MountSource
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("cannot find mountpoint %q", mountpoint)
	}

	// TODO:UC20: if the mountpoint is of a decrypted mapper device, then we
	//            need to trace back from the decrypted mapper device through
	//            luks to find the real encrypted partition underneath the
	//            decrypted one and thus the disk device for that partition

	// now we have the partition for this mountpoint, we need to tie that back
	// to a disk with a major minor, so query udev with the mount source path
	// of the mountpoint for properties
	props, err := udevProperties(mountpointPart.path)
	if err != nil && props == nil {
		// only fail here if props is nil, if it's available we validate it
		// below
		return nil, fmt.Errorf("cannot find disk for partition %s: %v", mountpointPart.path, err)
	}

	// ID_PART_ENTRY_DISK will give us the major and minor of the disk that this
	// partition originated from
	if majorMinor, ok := props["ID_PART_ENTRY_DISK"]; ok {
		maj, min, err := parseDeviceMajorMinor(majorMinor)
		if err != nil {
			// bad udev output?
			return nil, fmt.Errorf("cannot find disk for partition %s, bad udev output: %v", mountpointPart.path, err)
		}
		d.major = maj
		d.minor = min
	} else {
		// didn't find the property we need
		return nil, fmt.Errorf("cannot find disk for partition %s, incomplete udev output", mountpointPart.path)
	}

	return d, nil

}

func (d *disk) FindMatchingPartitionUUID(label string) (string, error) {
	// if we haven't found the partitions for this disk yet, do that now
	if d.partitions == nil {
		// step 1. find all devices with a matching major number
		// step 2. start at the major + minor device for the disk, and iterate over
		//         all devices that have a partition attribute, starting with the
		//         device with major same as disk and minor equal to disk minor + 1
		// step 3. if we hit a device that does not have a partition attribute, then
		//         we hit another disk, and shall stop searching

		// TODO: are there devices that have structures on them that show up as
		//       contiguous devices but are _not_ partitions, i.e. some littlekernel
		//       devices?

		// start with the minor + 1, since the major + minor of the disk we have
		// itself is not a partition
		currentMinor := d.minor
		for {
			currentMinor++
			partMajMin := fmt.Sprintf("%d:%d", d.major, currentMinor)
			props, err := udevProperties(filepath.Join("/dev/block", partMajMin))
			if err != nil && strings.Contains(err.Error(), "Unknown device") {
				// the device doesn't exist, we hit the end of the disk
				break
			} else if err != nil {
				// some other error trying to get udev properties, we should fail
				return "", fmt.Errorf("cannot get udev properties for partition %s: %v", partMajMin, err)
			}

			if props["DEVTYPE"] != "partition" {
				// we ran into another disk, break out
				break
			}

			p := &partition{
				major: d.major,
				minor: currentMinor,
			}

			if label := props["ID_FS_LABEL"]; label != "" {
				p.label = label
			} else {
				// this partition does not have a filesystem, and thus doesn't have
				// a filesystem label - this is not fatal, i.e. the bios-boot
				// partition does not have a filesystem label but it is the first
				// structure and so we should just skip it
				continue
			}

			if partuuid := props["ID_PART_ENTRY_UUID"]; partuuid != "" {
				p.partuuid = partuuid
			} else {
				return "", fmt.Errorf("cannot get udev properties for partition %s, missing udev property \"ID_PART_ENTRY_UUID\"", partMajMin)
			}

			d.partitions = append(d.partitions, p)

		}
	}

	// if we didn't find any partitions from above then return an error
	if len(d.partitions) == 0 {
		return "", fmt.Errorf("no partitions found for disk %s", d.Dev())
	}

	// iterate over the partitions looking for the specified label
	for _, part := range d.partitions {
		if part.label == label {
			return part.partuuid, nil
		}
	}

	return "", fmt.Errorf("couldn't find label %q", label)
}

func (d *disk) MountPointIsFromDisk(mountpoint string, opts *Options) (bool, error) {
	d2, err := diskFromMountPointImpl(mountpoint, opts)
	if err != nil {
		return false, err
	}

	// compare if the major/minor devices are the same
	return d.major == d2.major && d.minor == d2.minor, nil
}

func (d *disk) Dev() string {
	return fmt.Sprintf("%d:%d", d.major, d.minor)
}
