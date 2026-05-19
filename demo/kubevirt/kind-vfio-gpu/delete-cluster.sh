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

# Tears down the kind-vfio-gpu cluster and unloads the host-side
# fake-pci + fake-iommu kernel modules. Reverse of create-cluster.sh.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

set -e
set -o pipefail

: "${KIND_CLUSTER_NAME:=vfio-gpu-cluster}"
: "${KIND_CLUSTER_CONFIG_PATH:=${CURRENT_DIR}/kind-cluster-config.yaml}"
: "${KEEP_MODULES:=false}"
export KIND_CLUSTER_NAME KIND_CLUSTER_CONFIG_PATH

source "${CURRENT_DIR}/../../scripts/common.sh"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'
log_info() { printf "${GREEN}[INFO]${NC} %s\n" "$*"; }
log_warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }

SETUP_SCRIPT="${CURRENT_DIR}/setup-fake-pci-host.sh"

# 1. Delete the kind cluster. No-op if the cluster does not exist.
if ${KIND} get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    log_info "Deleting kind cluster ${KIND_CLUSTER_NAME}"
    ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}"
else
    log_info "Kind cluster ${KIND_CLUSTER_NAME} not found; skipping kind delete."
fi

# 1b. Clean any leftover kubeconfig context so the next 'kubectl' on the
#     host doesn't try to talk to the now-dead random API server port.
kubectl config delete-context "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
kubectl config delete-cluster "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
kubectl config delete-user    "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true

# 2. Unload host-side kernel modules.
if [[ "${KEEP_MODULES}" == "true" ]]; then
    log_warn "KEEP_MODULES=true - leaving fake-iommu / fake-pci loaded on the host."
else
    if [[ "$(uname -s)" == "Linux" ]]; then
        log_info "Unloading fake-pci + fake-iommu on the host"
        sudo bash "${SETUP_SCRIPT}" cleanup
    else
        log_warn "Not on Linux ($(uname -s)); skipping kernel-module unload."
    fi
fi

printf "${GREEN}Cluster teardown complete: ${KIND_CLUSTER_NAME}${NC}\n"
