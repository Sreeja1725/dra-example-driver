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

import "fmt"

// Validate ensures VfioMode is one of the recognized values.
func (m VfioMode) Validate() error {
	switch m {
	case "", VfioModePassthrough:
		return nil
	}
	return fmt.Errorf("unknown vfio mode: %v", m)
}

// Validate ensures the VfioConfig has a usable shape.
func (c *VfioConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("vfio config is nil")
	}
	return c.Mode.Validate()
}
