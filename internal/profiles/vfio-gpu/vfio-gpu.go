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
//
// KEP-5304 device-metadata files (the per-claim JSON KubeVirt's
// virt-launcher reads to learn the allocated BDF) are written by
// the upstream kubeletplugin framework, not by this profile. The
// driver enables that path with kubeletplugin.EnableDeviceMetadata
// and populates kubeletplugin.Device.Metadata.Attributes from the
// allocatable pool in cmd/dra-example-kubeletplugin/driver.go.
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

func (p Profile) ApplyConfig(config runtime.Object, _ []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
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
	return nil, nil
}

