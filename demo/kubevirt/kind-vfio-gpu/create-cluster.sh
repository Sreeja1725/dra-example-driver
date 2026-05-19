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

# Brings up a kind cluster wired for the vfio-gpu DRA profile against
# synthetic vfio-pci devices supplied by fake-iommu + fake-pci.
#
# Modeled after kubevirtci's cluster-up/cluster/kind-1.35-vgpu
# (provider.sh + config_vgpu_cluster.sh + vgpu-node/node.sh). High-level
# flow:
#
#   1. Pre-flight (Linux, kernel headers, kind, docker, sudo).
#   2. Build fake-iommu.ko + fake-pci.ko on the host.
#   3. Host-side setup: load fake-iommu, load fake-pci, bind vfio-pci to
#      the synthetic devices (delegated to setup-fake-pci-host.sh, our
#      equivalent of KubeVirt's pre-existing-host-GPU setup).
#   4. Delegate kind node-image build + cluster creation + (optional)
#      driver-image load to ../kind/create-cluster.sh, which already
#      handles all three. KIND_CLUSTER_NAME and KIND_CLUSTER_CONFIG_PATH
#      are exported below so the delegated script picks them up via
#      demo/scripts/common.sh.
#   5. Run config-vfio-cluster.sh to do the post-create per-node config
#      (remount /sys rw, chmod /dev/vfio/vfio) - this is the
#      configure-after-creation step KubeVirt's config_vgpu_cluster.sh
#      handles.
#   6. Install KubeVirt + enable the DRA feature gates on the KubeVirt CR.
#
# Re-running the script is safe; each step is idempotent.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

set -e
set -o pipefail

# Override cluster name + kind config BEFORE sourcing common.sh so the
# demo defaults pick our values up.
: "${KIND_CLUSTER_NAME:=vfio-gpu-cluster}"
: "${KIND_CLUSTER_CONFIG_PATH:=${CURRENT_DIR}/kind-cluster-config.yaml}"
export KIND_CLUSTER_NAME KIND_CLUSTER_CONFIG_PATH

source "${CURRENT_DIR}/../../scripts/common.sh"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info() { printf "${GREEN}[INFO]${NC} %s\n" "$*"; }
log_warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
log_err()  { printf "${RED}[ERROR]${NC} %s\n" "$*" >&2; }
die()      { log_err "$*"; exit 1; }

FAKE_PCI_DIR="${CURRENT_DIR}/fake-pci"
FAKE_IOMMU_DIR="${CURRENT_DIR}/fake-iommu"
SETUP_SCRIPT="${CURRENT_DIR}/setup-fake-pci-host.sh"
CONFIG_SCRIPT="${CURRENT_DIR}/config-vfio-cluster.sh"
: "${KIND_NODE_IMAGE:=kindest/node:v1.35.0}"
export KIND_NODE_IMAGE


# ---------- 1. pre-flight ----------
preflight() {
    log_info "Pre-flight checks"

    if [[ "$(uname -s)" != "Linux" ]]; then
        die "Host is $(uname -s). fake-pci / fake-iommu are kernel modules" \
            "and need a Linux host kernel you can insmod into." \
            "macOS Docker Desktop's Linux VM does not permit loading custom modules."
    fi

    command -v sudo   >/dev/null || die "sudo not found in PATH"
    command -v make   >/dev/null || die "make not found in PATH (need build tools to compile the kernel modules)"
    command -v gcc    >/dev/null || die "gcc not found in PATH (need build tools to compile the kernel modules)"
    command -v kind   >/dev/null || die "kind not found in PATH"
    command -v docker >/dev/null || die "docker not found in PATH"

    local kdir="/lib/modules/$(uname -r)/build"
    if [[ ! -d "${kdir}" ]]; then
        log_err "Kernel headers for $(uname -r) not found at ${kdir}."
        log_err "Install them first:"
        log_err "  Debian/Ubuntu:  sudo apt-get install linux-headers-\$(uname -r)"
        log_err "  Fedora/RHEL:    sudo dnf install kernel-devel-\$(uname -r)"
        exit 1
    fi

    if ! sudo -n true 2>/dev/null; then
        log_info "Priming sudo credentials (you'll be prompted once for the rest of the run)"
        sudo -v || die "sudo authentication failed"
    fi
}

# ---------- 2. build modules ----------
build_modules() {
    local m name
    for m in "${FAKE_IOMMU_DIR}" "${FAKE_PCI_DIR}"; do
        name="$(basename "${m}")"
        log_info "Cleaning ${name}"
        make -C "${m}" clean >/dev/null
        log_info "Building ${name}.ko"
        make -C "${m}" >/dev/null
    done
}

# ---------- 3. host setup: load modules + bind vfio-pci ----------
setup_host() {
    log_info "Host setup: load fake-iommu + fake-pci + bind vfio-pci"
    sudo bash "${SETUP_SCRIPT}" setup
    sudo bash "${SETUP_SCRIPT}" bind-vfio

    if ! ls /dev/vfio/* >/dev/null 2>&1; then
        die "/dev/vfio is empty after bind-vfio - check 'sudo ${SETUP_SCRIPT} status' and dmesg"
    fi
}

# ---------- 4. kind cluster ----------
create_kind_cluster() {
    if ${KIND} get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        log_warn "Cluster ${KIND_CLUSTER_NAME} already exists; reusing."
        log_warn "Delete it first with ./delete-cluster.sh for a fresh one."
    else
        log_info "Pulling pinned kind node image:"
        log_info "  ${KIND_NODE_IMAGE}"
        "${CONTAINER_TOOL}" pull "${KIND_NODE_IMAGE}"

        log_info "Creating kind cluster ${KIND_CLUSTER_NAME}"
        ${KIND} -v 9 create cluster \
            --name "${KIND_CLUSTER_NAME}" \
            --image "${KIND_NODE_IMAGE}" \
            --config "${KIND_CLUSTER_CONFIG_PATH}"
    fi

    log_info "Exporting kubeconfig for ${KIND_CLUSTER_NAME}"
    ${KIND} export kubeconfig --name "${KIND_CLUSTER_NAME}"

    log_info "Verifying API server is reachable"
    if ! kubectl --context "kind-${KIND_CLUSTER_NAME}" \
            cluster-info >/dev/null 2>&1; then
        log_err "API server for kind-${KIND_CLUSTER_NAME} is unreachable."
        log_err "  Likely a stale kind cluster. Run:"
        log_err "    ${KIND} delete cluster --name ${KIND_CLUSTER_NAME}"
        log_err "    docker ps -a --filter name=${KIND_CLUSTER_NAME}"
        die "aborting before kubevirt install"
    fi
    kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null
}

# ---------- 5. per-node post-create config ----------
configure_cluster() {
    log_info "Configuring nodes for vfio-pci (remount /sys rw, chmod /dev/vfio/vfio)"
    KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
        CONTAINER_TOOL="${CONTAINER_TOOL}" \
        bash "${CONFIG_SCRIPT}"
}

# ---------- 6. Install KubeVirt + DRA feature gates ----------
install_kubevirt() {
    # Guard against running with an empty kubeconfig - on previous
    # failures this used to spew the "error: current-context is not set"
    # noise after the real error.
    if ! kubectl config current-context >/dev/null 2>&1; then
        die "no current kubeconfig context - kind cluster create must have failed"
    fi

    log_info "Installing KubeVirt"
    # KUBEVIRT_CHANNEL selects which manifests we pull:
    #   stable        -> last tagged release (e.g. v1.8.2). MISSING PR
    #                    kubevirt/kubevirt#17028 ("VEP-10: use device specific
    #                    lookup ... resource claim template") which is required
    #                    by KEP-5304-style drivers.
    #   nightly       -> latest nightly main build. Has #17028 + everything since.
    #   <YYYYMMDD>    -> specific nightly date (e.g. 20260427). Resolved against
    #                    the same nightly bucket as 'nightly'.
    #   v<x.y.z>...   -> explicit released tag (e.g. v1.8.3 once it ships).
    #                    Resolved against GitHub releases.
    : "${KUBEVIRT_CHANNEL:=nightly}"

    local nightly_root="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt"
    local kv_channel kv_version kv_op_url kv_cr_url
    case "${KUBEVIRT_CHANNEL}" in
        stable)
            kv_channel="stable"
            kv_version=$(curl -fsSL https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt)
            kv_op_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-cr.yaml"
            ;;
        nightly)
            kv_channel="nightly"
            kv_version=$(curl -fsSL "${nightly_root}/latest")
            kv_op_url="${nightly_root}/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="${nightly_root}/${kv_version}/kubevirt-cr.yaml"
            ;;
        [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9])
            # 8-digit YYYYMMDD -> a specific nightly date.
            kv_channel="nightly"
            kv_version="${KUBEVIRT_CHANNEL}"
            kv_op_url="${nightly_root}/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="${nightly_root}/${kv_version}/kubevirt-cr.yaml"
            ;;
        v*)
            # Released tag like v1.8.2 / v1.8.3.
            kv_channel="release"
            kv_version="${KUBEVIRT_CHANNEL}"
            kv_op_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-cr.yaml"
            ;;
        *)
            die "Unrecognized KUBEVIRT_CHANNEL='${KUBEVIRT_CHANNEL}'." \
                "Expected one of: 'stable', 'nightly', an 8-digit nightly date" \
                "(e.g. 20260427), or a release tag (e.g. v1.8.3)."
            ;;
    esac
    log_info "  channel: ${kv_channel}"
    log_info "  version: ${kv_version}"

    # Fail fast with a clear message if the manifest URL is wrong - way
    # better than letting kubectl spit out a 404 page mid-install.
    if ! curl -fsI "${kv_op_url}" >/dev/null; then
        die "KubeVirt operator manifest not reachable: ${kv_op_url}" \
            "(check KUBEVIRT_CHANNEL='${KUBEVIRT_CHANNEL}')"
    fi

    kubectl create -f "${kv_op_url}"
    kubectl create -f "${kv_cr_url}"

    log_info "Waiting for KubeVirt to be Available (this can take 5-10 minutes)"
    kubectl -n kubevirt wait kv kubevirt --for condition=Available --timeout=600s

    log_info "Enabling DRA + HostDevices feature gates on the KubeVirt CR"
    kubectl patch kubevirt kubevirt -n kubevirt --type=merge -p \
        '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["GPUsWithDRA","HostDevicesWithDRA","HostDevices"]}}}}'
}

# ---------- main ----------
preflight
build_modules
setup_host
create_kind_cluster
configure_cluster
install_kubevirt