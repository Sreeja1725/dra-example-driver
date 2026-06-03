/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package vfiogpu

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultSysfsRoot is the kernel-supplied directory listing every PCI
// BDF currently bound to vfio-pci. Each entry is a symlink into
// /sys/devices/.../<BDF>/ where vendor, device, class, and iommu_group
// live.
const DefaultSysfsRoot = "/sys/bus/pci/drivers/vfio-pci"

var pciAddressRegexp = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

type SysfsDevice struct {
	// PCIAddress is the canonical BDF, e.g. "0000:65:00.0".
	PCIAddress string

	// VendorID is the PCI vendor ID as a lowercase 4-digit hex string
	// without the "0x" prefix, e.g. "10de".
	VendorID string

	// DeviceID is the PCI device ID as a lowercase 4-digit hex string
	// without the "0x" prefix, e.g. "20c2".
	DeviceID string

	// Class is the PCI class code as a lowercase 6-digit hex string
	// without the "0x" prefix, e.g. "030200". Optional - empty when
	// the kernel did not expose a class file.
	Class string

	// IommuGroup is the IOMMU group number the device is a member of.
	// Present whenever the host kernel has IOMMU enabled. We
	// defensively report -1 when the kernel symlink is missing rather
	// than failing the whole scan; vfio-pci passthrough cannot be
	// done for such devices.
	IommuGroup int64
}

// ScanSysfs walks `root` (typically [DefaultSysfsRoot]) and returns
// one [SysfsDevice] per PCI BDF symlink found.
// Devices are returned in lexicographic PCI-address order.
func ScanSysfs(root string) ([]SysfsDevice, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read vfio-gpu sysfs root %q: %w", root, err)
	}

	devices := make([]SysfsDevice, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !pciAddressRegexp.MatchString(name) {
			continue
		}

		devicePath := filepath.Join(root, name)
		dev, err := readPCIDevice(devicePath, name)
		if err != nil {
			continue
		}
		devices = append(devices, dev)
	}

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].PCIAddress < devices[j].PCIAddress
	})

	return devices, nil
}

// readPCIDevice resolves a single PCI sysfs entry under
// /sys/bus/pci/drivers/vfio-pci/. Its presence there is what makes
// this BDF passthrough-ready; per-device files (vendor, device,
// class, iommu_group) are read through the symlink into
// /sys/devices/.../<BDF>/.
func readPCIDevice(devicePath, address string) (SysfsDevice, error) {
	vendor, err := readHexFile(filepath.Join(devicePath, "vendor"))
	if err != nil {
		return SysfsDevice{}, fmt.Errorf("read vendor for %q: %w", address, err)
	}
	device, err := readHexFile(filepath.Join(devicePath, "device"))
	if err != nil {
		return SysfsDevice{}, fmt.Errorf("read device for %q: %w", address, err)
	}

	class, _ := readHexFile(filepath.Join(devicePath, "class"))

	iommuGroup, err := readPCIIommuGroup(devicePath)
	if err != nil {
		iommuGroup = -1
	}

	return SysfsDevice{
		PCIAddress: address,
		VendorID:   vendor,
		DeviceID:   device,
		Class:      class,
		IommuGroup: iommuGroup,
	}, nil
}

// readHexFile reads a sysfs file whose contents are a 0x-prefixed
// hexadecimal integer (e.g. "0xe1a5\n") and returns the lowercase hex
// digits without the prefix. Whitespace is trimmed.
func readHexFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.ToLower(strings.TrimSpace(string(raw)))
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return "", fmt.Errorf("empty hex value in %q", path)
	}
	return s, nil
}

// readPCIIommuGroup returns the IOMMU group number for a PCI device.
// The kernel exposes <devicePath>/iommu_group as a symlink to
// /sys/kernel/iommu_groups/<N>; we just take the basename of the
// target and parse it as a decimal integer.
func readPCIIommuGroup(devicePath string) (int64, error) {
	base, err := readSymlinkBasename(filepath.Join(devicePath, "iommu_group"))
	if err != nil {
		return 0, err
	}
	g, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse iommu group %q: %w", base, err)
	}
	return g, nil
}

// readSymlinkBasename resolves a symlink and returns the basename of
// its target.
func readSymlinkBasename(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}
