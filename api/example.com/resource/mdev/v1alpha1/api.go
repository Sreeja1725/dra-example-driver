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

const MdevConfigKind = "MdevConfig"

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MdevConfig is the opaque parameter object accepted by ResourceClaims and
// DeviceClasses targeting the "mdev" profile. The example driver does not
// perform real mediated-device creation, but it does let consumers select
// the mediated-device "type" (e.g. vfio-pci for vGPUs vs vfio-ap for s390x
// crypto adapters) so that downstream consumers like KubeVirt can pick the
// correct libvirt model when assembling the host device.
type MdevConfig struct {
	metav1.TypeMeta `json:",inline"`

	// Type selects the mediated-device subsystem model that consumers should
	// use. Currently recognized values are "vfio-pci" (vGPUs, default) and
	// "vfio-ap" (s390x crypto adapters).
	Type MdevType `json:"type,omitempty"`
}

// MdevType enumerates the mediated-device subsystem models surfaced by the
// example driver.
type MdevType string

const (
	// MdevTypeVfioPCI is the default vGPU-style mediated device.
	MdevTypeVfioPCI MdevType = "vfio-pci"
	// MdevTypeVfioAP models the s390x crypto adapter mediated device.
	MdevTypeVfioAP MdevType = "vfio-ap"
)

// DefaultMdevConfig returns the default configuration applied when a
// ResourceClaim does not provide its own opaque parameters.
func DefaultMdevConfig() *MdevConfig {
	return &MdevConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       MdevConfigKind,
		},
		Type: MdevTypeVfioPCI,
	}
}

// Normalize fills in implied defaults.
func (c *MdevConfig) Normalize() error {
	if c == nil {
		return nil
	}
	if c.Type == "" {
		c.Type = MdevTypeVfioPCI
	}
	return nil
}
