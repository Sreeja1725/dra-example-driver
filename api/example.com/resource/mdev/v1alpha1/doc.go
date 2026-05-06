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

// +k8s:deepcopy-gen=package
// +groupName=mdev.resource.example.com

// Package v1alpha1 holds the opaque-config types for the "mdev" device profile.
//
// The mdev profile advertises mock mediated devices (vGPU / vfio-ap style)
// so that consumers (in particular KubeVirt VirtualMachineInstances) can
// exercise the DRA HostDevice flow for mediated devices end-to-end without
// real hardware. Devices expose an "mdevUUID" attribute that virt-launcher
// reads from the KEP-5304 metadata file produced by the kubeletplugin.
package v1alpha1
