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

// Package mdev implements the "mdev" device profile for the dra-example
// driver. It advertises a configurable number of mock mediated devices
// (vGPU / vfio-ap style) and exposes the well-known KubeVirt attribute
// "mdevUUID" so virt-launcher can build the corresponding host device
// stanza without real hardware. The profile is otherwise structurally
// identical to the vfio profile and shares the same KEP-5304 metadata file
// layout.
package mdev

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

	configapi "sigs.k8s.io/dra-example-driver/api/example.com/resource/mdev/v1alpha1"
	"sigs.k8s.io/dra-example-driver/internal/profiles"
)

// ProfileName identifies the mdev profile to the driver and helm chart.
const ProfileName = "mdev"

// Well-known DRA device attribute keys consumed by KubeVirt's
// pkg/dra/metadata package.
const (
	// MDevUUIDAttribute carries the mediated device UUID that virt-launcher
	// uses when building libvirt's <hostdev type='mdev'> element.
	MDevUUIDAttribute resourceapi.QualifiedName = "mdevUUID"

	// PCIBusIDAttribute is included so that consumers which key off
	// pciBusID (e.g. the parent device's address) can still discover it.
	PCIBusIDAttribute resourceapi.QualifiedName = "resource.kubernetes.io/pciBusID"

	// MDevTypeAttribute records the libvirt model that consumers should
	// use ("vfio-pci" or "vfio-ap").
	MDevTypeAttribute resourceapi.QualifiedName = "resource.kubernetes.io/mdevType"
)

const (
	metadataAPIVersion           = "metadata.resource.k8s.io/v1alpha1"
	metadataKind                 = "DeviceMetadata"
	metadataContainerMountPath   = "/var/run/kubernetes.io/dra-device-attributes"
	metadataResourceClaimsSubdir = "resourceclaims"
	metadataFileSuffix           = "-metadata.json"
	metadataDirName              = "dra-metadata"
)

// Profile is the mdev device profile.
type Profile struct {
	nodeName     string
	numDevices   int
	driverName   string
	metadataRoot string
}

// NewProfile constructs an mdev Profile.
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
	rng := rand.New(rand.NewSource(hash(p.nodeName) ^ 0x6d646576))

	devices := make([]resourceapi.Device, 0, p.numDevices)
	for i := 0; i < p.numDevices; i++ {
		mdevUUID := generateUUID(rng)
		// All mdevs in this mock share the same parent PCI device. This is
		// realistic for vGPUs where one physical card hosts many mediated
		// children.
		parentPCI := fmt.Sprintf("0000:%02x:00.0", 0x80+(i/16))

		devices = append(devices, resourceapi.Device{
			Name: fmt.Sprintf("mdev-%d", i),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"index":           {IntValue: ptr.To(int64(i))},
				"model":           {StringValue: ptr.To("LATEST-MDEV-MODEL")},
				"driverVersion":   {VersionValue: ptr.To("1.0.0")},
				MDevUUIDAttribute: {StringValue: ptr.To(mdevUUID)},
				PCIBusIDAttribute: {StringValue: ptr.To(parentPCI)},
				MDevTypeAttribute: {StringValue: ptr.To(string(configapi.MdevTypeVfioPCI))},
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
	cfg, ok := config.(*configapi.MdevConfig)
	if !ok {
		return fmt.Errorf("expected v1alpha1.MdevConfig but got: %T", config)
	}
	return cfg.Validate()
}

// ApplyConfig implements [profiles.ConfigHandler].
func (p Profile) ApplyConfig(config runtime.Object, _ []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config == nil {
		config = configapi.DefaultMdevConfig()
	}
	cfg, ok := config.(*configapi.MdevConfig)
	if !ok {
		return nil, fmt.Errorf("runtime object is not a recognized configuration: %T", config)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("error normalizing mdev config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("error validating mdev config: %w", err)
	}
	return nil, nil
}

// PrepareClaim implements [profiles.Profile].
func (p Profile) PrepareClaim(claim *resourceapi.ResourceClaim, allocatable map[string]resourceapi.Device, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if len(results) == 0 {
		return nil, nil
	}

	claimUID := string(claim.UID)
	claimRefName := claim.Name

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
		filePath := filepath.Join(hostDir, p.driverName+metadataFileSuffix)
		if err := writeMetadataFile(filePath, md); err != nil {
			return nil, err
		}

		mount := metadataMount(hostDir, claimRefName, request)
		for _, r := range requestResults {
			merged := edits[r.Device]
			if merged == nil {
				merged = &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}}
			}
			merged.Append(&cdiapi.ContainerEdits{
				ContainerEdits: &cdispec.ContainerEdits{
					Mounts: []*cdispec.Mount{mount},
					Env: []string{
						fmt.Sprintf("MDEV_DEVICE_%s_UUID=%s", r.Device, deviceAttributeString(allocatable, r.Device, MDevUUIDAttribute)),
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

func metadataMount(hostDir, claimName, requestName string) *cdispec.Mount {
	return &cdispec.Mount{
		HostPath:      hostDir,
		ContainerPath: filepath.Join(metadataContainerMountPath, metadataResourceClaimsSubdir, claimName, requestName),
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
	Driver     string                                                    `json:"driver"`
	Pool       string                                                    `json:"pool"`
	Name       string                                                    `json:"name"`
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute `json:"attributes,omitempty"`
}
