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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-example-driver/api/example.com/resource/vfio-gpu/v1alpha1"
	"sigs.k8s.io/dra-example-driver/internal/profiles"
)

// ProfileName identifies the vfio-gpu profile to the driver and helm
// chart. The helm chart's `deviceProfile` value must use this exact
// string; the auto-derived driver name is "vfio-gpu.example.com".
const ProfileName = "vfio-gpu"

// Well-known DRA device attribute keys consumed by KubeVirt's
// pkg/dra/metadata package. Mirrored here so the driver does not need
// a build-time dependency on the KubeVirt module.
//
// Only [PCIBusIDAttribute] is part of KubeVirt's contract: virt-launcher
// reads it from the per-claim KEP-5304 metadata file (written by the
// upstream kubeletplugin framework when the driver is started with
// EnableDeviceMetadata) and uses it to build
// `-device vfio-pci,host=<BDF>` for QEMU. Every other attribute below
// is informational - it ships into the metadata file for
// observability/debugging and lets advanced consumers (selectors,
// CEL expressions, custom controllers) match on vendor/device/class/
// topology, but no downstream consumer is required to look at them.
const (
	// PCIBusIDAttribute is the standard attribute carrying the host
	// PCI address of a passthrough device.
	PCIBusIDAttribute resourceapi.QualifiedName = "resource.kubernetes.io/pciBusID"

	// VendorIDAttribute is the PCI vendor ID of the device.
	VendorIDAttribute resourceapi.QualifiedName = "VendorID"

	// DeviceIDAttribute is the PCI device ID of the device.
	DeviceIDAttribute resourceapi.QualifiedName = "DeviceID"

	// ClassAttribute is the PCI class code of the device.
	ClassAttribute resourceapi.QualifiedName = "Class"

	// IommuGroupAttribute is the IOMMU group number for the device.
	//
	// Optional / informational from KubeVirt's point of view -
	// virt-launcher derives nothing from it.
	IommuGroupAttribute resourceapi.QualifiedName = "iommuGroup"
)

// Profile is the vfio-gpu device profile. It advertises one DRA
// device per PCI BDF symlink found under [Profile.sysfsRoot] (the
// kernel-supplied list of devices already bound to vfio-pci) and
// surfaces their attributes (PCI bus ID, vendor/device/class, IOMMU
// group) in the published ResourceSlice.
type Profile struct {
	nodeName       string
	driverName     string
	sysfsRoot      string
	pciDevicesRoot string
}

// NewProfile constructs a vfio-gpu Profile.
func NewProfile(nodeName, driverName, sysfsRoot, pciDevicesRoot string) Profile {
	if sysfsRoot == "" {
		sysfsRoot = DefaultSysfsRoot
	}
	if pciDevicesRoot == "" {
		pciDevicesRoot = DefaultPCIDevicesRoot
	}
	return Profile{
		nodeName:       nodeName,
		driverName:     driverName,
		sysfsRoot:      sysfsRoot,
		pciDevicesRoot: pciDevicesRoot,
	}
}

// deviceName returns the DRA device name assigned to the i-th sysfs
// scan result. EnumerateDevices and ApplyConfig must agree on this
// convention so that ApplyConfig can map a kubelet-supplied result.Device
// back to a SysfsDevice.
func deviceName(index int) string {
	return fmt.Sprintf("pci-%d", index)
}

// scanByName runs ScanSysfs against the profile's configured roots and
// returns the results keyed by the DRA device name that EnumerateDevices
// assigned. Used by ApplyConfig to recover per-device sysfs facts
// (notably IOMMU group) from a kubelet-supplied result.Device string.
func (p Profile) scanByName() (map[string]SysfsDevice, error) {
	scanned, err := ScanSysfs(p.sysfsRoot, p.pciDevicesRoot)
	if err != nil {
		return nil, fmt.Errorf("scan vfio-pci sysfs at %q: %w", p.sysfsRoot, err)
	}
	out := make(map[string]SysfsDevice, len(scanned))
	for i, s := range scanned {
		out[deviceName(i)] = s
	}
	return out, nil
}

// EnumerateDevices implements [profiles.Profile]. It scans the
// configured vfio-gpu sysfs tree and returns one DRA device per BDF.
func (p Profile) EnumerateDevices() (resourceslice.DriverResources, error) {
	scanned, err := ScanSysfs(p.sysfsRoot, p.pciDevicesRoot)
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("scan vfio-pci sysfs at %q: %w", p.sysfsRoot, err)
	}

	devices := make([]resourceapi.Device, 0, len(scanned))
	for i, s := range scanned {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"index":           {IntValue: ptr.To(int64(i))},
			PCIBusIDAttribute: {StringValue: ptr.To(s.PCIAddress)},
			VendorIDAttribute: {StringValue: ptr.To(s.VendorID)},
			DeviceIDAttribute: {StringValue: ptr.To(s.DeviceID)},
			"driverVersion":   {VersionValue: ptr.To("1.0.0")},
		}

		if s.Class != "" {
			attrs[ClassAttribute] = resourceapi.DeviceAttribute{
				StringValue: ptr.To(s.Class),
			}
		}
		if s.IommuGroup >= 0 {
			attrs[IommuGroupAttribute] = resourceapi.DeviceAttribute{
				IntValue: ptr.To(s.IommuGroup),
			}
		}

		devices = append(devices, resourceapi.Device{
			Name:       fmt.Sprintf("pci-%d", i),
			Attributes: attrs,
		})
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}, nil
}

func (p Profile) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder(configapi.AddToScheme)
}

func (p Profile) Validate(config runtime.Object) error {
	cfg, ok := config.(*configapi.VfioConfig)
	if !ok {
		return fmt.Errorf("expected v1alpha1.VfioConfig but got: %T", config)
	}
	return cfg.Validate()
}

func (p Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config == nil {
		config = configapi.DefaultVfioConfig()
	}
	cfg, ok := config.(*configapi.VfioConfig)
	if !ok {
		return nil, fmt.Errorf("runtime object is not a recognized configuration: %T", config)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("error normalizing vfio config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("error validating vfio config: %w", err)
	}

	if len(results) == 0 {
		return nil, nil
	}

	devices, err := p.scanByName()
	if err != nil {
		return nil, err
	}

	perDeviceEdits := make(profiles.PerDeviceCDIContainerEdits, len(results))
	for _, result := range results {
		dev, ok := devices[result.Device]
		if !ok {
			return nil, fmt.Errorf("vfio-gpu sysfs scan no longer sees allocated device %q (currently visible: %d); was it unbound from vfio-pci?", result.Device, len(devices))
		}
		if dev.IommuGroup < 0 {
			return nil, fmt.Errorf("vfio-gpu device %q (BDF %s) has no IOMMU group; the kernel must be booted with intel_iommu=on / amd_iommu=on for vfio-pci passthrough", result.Device, dev.PCIAddress)
		}

		edits := &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{
				{Path: fmt.Sprintf("/dev/vfio/%d", dev.IommuGroup), Permissions: "rwm"},
				{Path: "/dev/vfio/vfio", Permissions: "rwm"},
			},
		}
		perDeviceEdits[result.Device] = &cdiapi.ContainerEdits{ContainerEdits: edits}
	}

	return perDeviceEdits, nil
}
