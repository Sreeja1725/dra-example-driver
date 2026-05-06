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

// Package vfio implements the "vfio" device profile for the dra-example
// driver. It advertises a configurable number of mock vfio-pci devices on
// the local node, exposing the well-known KubeVirt attribute
// "resource.kubernetes.io/pciBusID" so that virt-launcher can reconstruct
// the host device when it consumes the claim.
//
// During NodePrepareResources the profile writes a KEP-5304-style metadata
// file under the plugin's data directory and emits a CDI mount edit that
// surfaces the directory inside any consuming pod at the path
// "/var/run/kubernetes.io/dra-device-attributes" - the same path
// kubevirt.io/kubevirt/pkg/dra reads from.
package vfio

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-example-driver/api/example.com/resource/vfio/v1alpha1"
	"sigs.k8s.io/dra-example-driver/internal/profiles"
)

// ProfileName identifies the vfio profile to the driver and helm chart.
const ProfileName = "vfio"

// Well-known DRA device attribute keys consumed by KubeVirt's
// pkg/dra/metadata package. Mirrored here so the driver does not need a
// build-time dependency on the KubeVirt module.
const (
	// PCIBusIDAttribute is the standard attribute carrying the host PCI
	// address of a passthrough device (KEP-5304).
	PCIBusIDAttribute resourceapi.QualifiedName = "resource.kubernetes.io/pciBusID"

	// VendorIDAttribute is a conventional attribute describing the PCI
	// vendor ID of the device. Optional - included for parity with what
	// real drivers expose.
	VendorIDAttribute resourceapi.QualifiedName = "resource.kubernetes.io/pciVendorID"

	// DeviceIDAttribute is a conventional attribute describing the PCI
	// device ID of the device. Optional - included for parity with what
	// real drivers expose.
	DeviceIDAttribute resourceapi.QualifiedName = "resource.kubernetes.io/pciDeviceID"
)

// Constants describing the KubeVirt-readable metadata layout. These mirror
// the values in kubevirt.io/kubevirt/pkg/dra/metadata; we redefine them
// locally to avoid pulling KubeVirt into the example driver's module graph.
const (
	metadataAPIVersion           = "metadata.resource.k8s.io/v1alpha1"
	metadataKind                 = "DeviceMetadata"
	metadataContainerMountPath   = "/var/run/kubernetes.io/dra-device-attributes"
	metadataResourceClaimsSubdir = "resourceclaims"
	metadataFileSuffix           = "-metadata.json"
	metadataDirName              = "dra-metadata"
)

// Profile is the vfio device profile.
type Profile struct {
	nodeName     string
	numDevices   int
	driverName   string
	metadataRoot string
}

// NewProfile constructs a vfio Profile.
//
// `metadataRoot` is the directory on the host where per-claim metadata
// files will be written. CDI mount edits will reference subdirectories of
// this path so they are bind-mounted into consuming pods.
func NewProfile(nodeName, driverName, metadataRoot string, numDevices int) Profile {
	return Profile{
		nodeName:     nodeName,
		numDevices:   numDevices,
		driverName:   driverName,
		metadataRoot: metadataRoot,
	}
}

// EnumerateDevices implements [profiles.Profile].
func (p Profile) EnumerateDevices() (resourceslice.DriverResources, error) {
	rng := rand.New(rand.NewSource(hash(p.nodeName)))

	devices := make([]resourceapi.Device, 0, p.numDevices)
	for i := 0; i < p.numDevices; i++ {
		// Deterministic but plausible-looking PCI address. We start the
		// bus high (0x80+) to avoid colliding with anything realistic that
		// might exist on a kind node.
		pciBusID := fmt.Sprintf("0000:%02x:%02x.0", 0x80+(i/8), i%8)
		deviceUUID := generateUUID(rng)

		devices = append(devices, resourceapi.Device{
			Name: fmt.Sprintf("vfio-%d", i),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"index":            {IntValue: ptr.To(int64(i))},
				"uuid":             {StringValue: ptr.To(deviceUUID)},
				"model":            {StringValue: ptr.To("LATEST-VFIO-MODEL")},
				"driverVersion":    {VersionValue: ptr.To("1.0.0")},
				PCIBusIDAttribute:  {StringValue: ptr.To(pciBusID)},
				VendorIDAttribute:  {StringValue: ptr.To("10de")},
				DeviceIDAttribute:  {StringValue: ptr.To(fmt.Sprintf("20%02x", i))},
			},
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

// SchemeBuilder implements [profiles.ConfigHandler].
func (p Profile) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder(configapi.AddToScheme)
}

// Validate implements [profiles.ConfigHandler].
func (p Profile) Validate(config runtime.Object) error {
	cfg, ok := config.(*configapi.VfioConfig)
	if !ok {
		return fmt.Errorf("expected v1alpha1.VfioConfig but got: %T", config)
	}
	return cfg.Validate()
}

// ApplyConfig implements [profiles.ConfigHandler].
//
// The vfio profile does not currently inject any per-config CDI edits: the
// only edits we need are the metadata mount, which is added in
// PrepareClaim because it is keyed off the claim (not the config). We still
// validate the config shape here so that bad opaque parameters are caught
// early.
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

// PrepareClaim implements [profiles.Profile].
//
// For each request in the claim allocation it:
//  1. writes a KEP-5304-style metadata file containing the attributes of
//     every device allocated to the request, and
//  2. emits a per-device CDI mount edit so that consumer pods see the file
//     at the path KubeVirt's virt-launcher expects.
func (p Profile) PrepareClaim(claim *resourceapi.ResourceClaim, allocatable map[string]resourceapi.Device, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if len(results) == 0 {
		return nil, nil
	}

	claimUID := string(claim.UID)
	claimRefName, claimSubdir := claimMetadataLocation(claim)

	byRequest := make(map[string][]*resourceapi.DeviceRequestAllocationResult)
	for _, r := range results {
		byRequest[r.Request] = append(byRequest[r.Request], r)
	}

	edits := make(profiles.PerDeviceCDIContainerEdits)
	for request, requestResults := range byRequest {
		hostDir := filepath.Join(p.metadataRoot, metadataDirName, claimUID, request)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return nil, fmt.Errorf("create metadata dir %q: %w", hostDir, err)
		}

		md := buildDeviceMetadata(p.driverName, claimRefName, requestResults, allocatable)
		fileName := p.driverName + metadataFileSuffix
		filePath := filepath.Join(hostDir, fileName)
		if err := writeMetadataFile(filePath, md); err != nil {
			return nil, err
		}

		mount := metadataMount(hostDir, claimSubdir, claimRefName, request)
		for _, r := range requestResults {
			merged := edits[r.Device]
			if merged == nil {
				merged = &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}}
			}
			merged.Append(&cdiapi.ContainerEdits{
				ContainerEdits: &cdispec.ContainerEdits{
					Mounts: []*cdispec.Mount{mount},
					Env: []string{
						fmt.Sprintf("VFIO_DEVICE_%s_PCI_BUS_ID=%s", r.Device, deviceAttributeString(allocatable, r.Device, PCIBusIDAttribute)),
					},
				},
			})
			edits[r.Device] = merged
		}
	}

	return edits, nil
}

// UnprepareClaim implements [profiles.ClaimUnpreparer]. It removes the
// per-claim metadata directory written during PrepareClaim. Errors that
// indicate the directory was already removed are treated as success.
func (p Profile) UnprepareClaim(claimUID string) error {
	dir := filepath.Join(p.metadataRoot, metadataDirName, claimUID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove metadata dir %q: %w", dir, err)
	}
	return nil
}

// claimMetadataLocation returns the (claimName, subdir) pair used by
// KubeVirt's pkg/dra to locate the metadata file. Direct ResourceClaims
// land under "resourceclaims/<claim-name>"; template-derived claims land
// under "resourceclaimtemplates/<pod-claim-name>", but template support is
// not yet wired in here - the example fixtures use direct claims.
func claimMetadataLocation(claim *resourceapi.ResourceClaim) (string, string) {
	return claim.Name, metadataResourceClaimsSubdir
}

func buildDeviceMetadata(driverName, claimName string, results []*resourceapi.DeviceRequestAllocationResult, allocatable map[string]resourceapi.Device) deviceMetadata {
	requestName := ""
	if len(results) > 0 {
		requestName = results[0].Request
	}

	devices := make([]deviceMetadataDevice, 0, len(results))
	for _, r := range results {
		dev := allocatable[r.Device]
		devices = append(devices, deviceMetadataDevice{
			Driver:     driverName,
			Pool:       r.Pool,
			Name:       r.Device,
			Attributes: dev.Attributes,
		})
	}

	return deviceMetadata{
		APIVersion: metadataAPIVersion,
		Kind:       metadataKind,
		Metadata:   metadataObjectMeta{Name: claimName},
		Requests: []deviceMetadataRequest{{
			Name:    requestName,
			Devices: devices,
		}},
	}
}

func writeMetadataFile(path string, md deviceMetadata) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "metadata-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp metadata file in %q: %w", filepath.Dir(path), err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(md); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp metadata file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename metadata file into place: %w", err)
	}
	return nil
}

func metadataMount(hostDir, subdir, claimName, requestName string) *cdispec.Mount {
	return &cdispec.Mount{
		HostPath:      hostDir,
		ContainerPath: filepath.Join(metadataContainerMountPath, subdir, claimName, requestName),
		Type:          "bind",
		Options:       []string{"bind", "ro"},
	}
}

func deviceAttributeString(allocatable map[string]resourceapi.Device, deviceName string, attr resourceapi.QualifiedName) string {
	d, ok := allocatable[deviceName]
	if !ok {
		return ""
	}
	v, ok := d.Attributes[attr]
	if !ok || v.StringValue == nil {
		return ""
	}
	return *v.StringValue
}

func generateUUID(r *rand.Rand) string {
	var raw [16]byte
	r.Read(raw[:])
	id, _ := uuid.FromBytes(raw[:])
	return id.String()
}

func hash(s string) int64 {
	h := int64(0)
	for _, c := range s {
		h = 31*h + int64(c)
	}
	return h
}

// Local mirrors of the KEP-5304 metadata schema. We keep them here, instead
// of importing kubevirt.io/kubevirt/pkg/dra/metadata, so the example driver
// does not need a hard dependency on KubeVirt.
type deviceMetadata struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Metadata   metadataObjectMeta      `json:"metadata,omitempty"`
	Requests   []deviceMetadataRequest `json:"requests,omitempty"`
}

type metadataObjectMeta struct {
	Name string `json:"name,omitempty"`
}

type deviceMetadataRequest struct {
	Name    string                 `json:"name"`
	Devices []deviceMetadataDevice `json:"devices,omitempty"`
}

type deviceMetadataDevice struct {
	Driver     string                                                       `json:"driver"`
	Pool       string                                                       `json:"pool"`
	Name       string                                                       `json:"name"`
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute    `json:"attributes,omitempty"`
}
