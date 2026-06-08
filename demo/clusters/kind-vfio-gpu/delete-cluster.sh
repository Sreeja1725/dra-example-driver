#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
_unused() { :; } # keep CURRENT_DIR for shellcheck symmetry with create-cluster.sh
_unused "${CURRENT_DIR}"

set -e
set -o pipefail

: "${KIND_CLUSTER_NAME:=kind-vfio-gpu}"

if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    echo "Deleting kind cluster ${KIND_CLUSTER_NAME}"
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
else
    echo "kind cluster ${KIND_CLUSTER_NAME} not found - skipping"
fi

kubectl config delete-context "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
kubectl config delete-cluster "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
kubectl config delete-user    "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true

printf "${GREEN}Cluster teardown complete: ${KIND_CLUSTER_NAME}${NC}\n"
printf "${YELLOW}NOTE:${NC} host vfio-pci bindings (and any fake-pci/fake-iommu modules)\n"
printf "      are left untouched. Unload them with the kubevirt-side teardown\n"
printf "      you used to set them up, e.g.:\n"
printf "          sudo bash <path-to>/setup-fake-pci-host.sh cleanup\n"
