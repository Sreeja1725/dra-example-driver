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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const VfioConfigKind = "VfioConfig"

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VfioConfig is the opaque parameter object accepted by ResourceClaims and
// DeviceClasses targeting the "vfio" profile. It is intentionally minimal:
// the example driver does not perform any real hardware configuration so the
// only knob exposed here is whether to run the device in a VM-friendly
// "passthrough" mode (the default and currently only supported value).
type VfioConfig struct {
	metav1.TypeMeta `json:",inline"`

	// Mode selects how the consumer intends to use the allocated device.
	// Currently only "Passthrough" is recognized.
	Mode VfioMode `json:"mode,omitempty"`
}

// VfioMode enumerates the supported consumption modes for a vfio device.
type VfioMode string

const (
	// VfioModePassthrough indicates the device should be exposed for VM
	// PCI passthrough (vfio-pci). This is the default.
	VfioModePassthrough VfioMode = "Passthrough"
)

// DefaultVfioConfig returns the default configuration applied when a
// ResourceClaim does not provide its own opaque parameters.
func DefaultVfioConfig() *VfioConfig {
	return &VfioConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       VfioConfigKind,
		},
		Mode: VfioModePassthrough,
	}
}

// Normalize fills in implied defaults.
func (c *VfioConfig) Normalize() error {
	if c == nil {
		return nil
	}
	if c.Mode == "" {
		c.Mode = VfioModePassthrough
	}
	return nil
}
