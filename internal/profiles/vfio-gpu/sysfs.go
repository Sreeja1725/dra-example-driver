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
// device currently bound to the vfio-pci driver. Each entry is a
// symlink whose name is the PCI BDF, e.g.:
//
//	/sys/bus/pci/drivers/vfio-pci/0000:65:00.0 ->
//	  ../../../../devices/pci0000:00/.../0000:65:00.0

const DefaultSysfsRoot = "/sys/bus/pci/drivers/vfio-pci"

// DefaultPCIDevicesRoot is the canonical, bus-wide PCI device
// directory. The scan reads vendor/device/class for each
// vfio-pci-bound BDF from <DefaultPCIDevicesRoot>/<BDF>/ rather
// than going through the drivers/vfio-pci symlink, so the source
// of truth for those informational attributes is the bus's
// per-device entry, not the driver binding.
const DefaultPCIDevicesRoot = "/sys/bus/pci/devices"

// pciAddressRegexp matches a canonical PCI BDF address, e.g.
// "0000:65:00.0" or "faca:00:00.0". Hex digits in domain/bus/slot;
// function is 0..7 (also hex but bounded).
var pciAddressRegexp = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// SysfsDevice is a single PCI device discovered under
// /sys/bus/pci/drivers/vfio-pci/ and the informational attributes from
// /sys/bus/pci/devices. By construction every such device
// is already bound to vfio-pci and therefore usable as a passthrough
// target; the struct carries the attributes the DRA driver needs to
// surface the device and wire up VFIO at NodePrepareResources time.
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

	// SysfsPath is the absolute path of the device entry under sysfs
	// (the symlink under /sys/bus/pci/drivers/vfio-pci/, not the
	// realpath). Useful for debugging and for env vars that
	// downstream consumers might want to read.
	SysfsPath string
}

// ScanSysfs walks `driversRoot` (typically [DefaultSysfsRoot]) and
// returns one [SysfsDevice] per PCI BDF symlink found. Kernel
// control files (`bind`, `unbind`, `new_id`, `remove_id`, `module`,
// `uevent`) that share the drivers/vfio-pci/ directory are skipped
// because their names do not match [pciAddressRegexp].
//
// For each surviving BDF, vendor/device/class are read from
// <devicesRoot>/<BDF>/* (typically [DefaultPCIDevicesRoot]) - that
// is, from the canonical per-device entry under /sys/bus/pci/devices/
// rather than via the drivers/vfio-pci/<BDF> symlink. The two paths
// resolve to the same realpath under /sys/devices/.../<BDF>/, but
// sourcing the informational attributes from the bus-wide list
// makes the data flow explicit: the drivers/vfio-pci/ tree tells
// us which BDFs are passthrough-ready; the devices/ tree tells us
// what those BDFs *are*. IOMMU group and NUMA node are still read
// via the drivers/vfio-pci/ path - they are the same files either
// way.
//
// Devices are returned in lexicographic PCI-address order so the
// resulting ResourceSlice is stable across pod restarts.
func ScanSysfs(driversRoot, devicesRoot string) ([]SysfsDevice, error) {
	if driversRoot == "" {
		driversRoot = DefaultSysfsRoot
	}
	if devicesRoot == "" {
		devicesRoot = DefaultPCIDevicesRoot
	}

	entries, err := os.ReadDir(driversRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read vfio-gpu sysfs root %q: %w", driversRoot, err)
	}

	devices := make([]SysfsDevice, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !pciAddressRegexp.MatchString(name) {
			continue
		}

		driverDevicePath := filepath.Join(driversRoot, name)
		busDevicePath := filepath.Join(devicesRoot, name)

		dev, err := readPCIDevice(driverDevicePath, busDevicePath, name)
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

// readPCIDevice resolves a single PCI sysfs entry.
//
// `driverDevicePath` is the path of the BDF symlink under
// /sys/bus/pci/drivers/vfio-pci/. Its presence there is what makes
// this BDF passthrough-ready, and the per-device files reachable
// through it (iommu_group, numa_node) live in /sys/devices/...
// /<BDF>/ via the symlink.
//
// Returns an error when the required vendor/device files are
// missing, so phantom entries (e.g. a stale symlink under
// drivers/vfio-pci/) don't surface as allocatable devices.
func readPCIDevice(driverDevicePath, busDevicePath, address string) (SysfsDevice, error) {
	vendor, err := readHexFile(filepath.Join(busDevicePath, "vendor"))
	if err != nil {
		return SysfsDevice{}, fmt.Errorf("read vendor for %q: %w", address, err)
	}
	device, err := readHexFile(filepath.Join(busDevicePath, "device"))
	if err != nil {
		return SysfsDevice{}, fmt.Errorf("read device for %q: %w", address, err)
	}

	class, _ := readHexFile(filepath.Join(busDevicePath, "class"))

	iommuGroup, err := readPCIIommuGroup(driverDevicePath)
	if err != nil {
		iommuGroup = -1
	}

	return SysfsDevice{
		PCIAddress: address,
		VendorID:   vendor,
		DeviceID:   device,
		Class:      class,
		IommuGroup: iommuGroup,
		SysfsPath:  driverDevicePath,
	}, nil
}

// readHexFile reads a sysfs file whose contents are a 0x-prefixed
// hexadecimal integer (e.g. "0x10de\n") and returns the lowercase hex
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

// readNumaNode reads <devicePath>/numa_node. Missing or unparseable
// values are reported as -1 (matching what the kernel writes for
// devices without an explicit NUMA association).
func readNumaNode(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// readSymlinkBasename resolves a symlink and returns the basename of
// its target. Both relative and absolute targets are supported.
func readSymlinkBasename(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}
