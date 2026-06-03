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

# Create a kind cluster on top of an already-vfio-prepared host, then
# install KubeVirt with the DRA + HostDevices feature gates and the
# dra-example-driver helm chart for the vfio-gpu profile.
#
# Prerequisite (handled by the user, not this script): the host already
# has devices bound to vfio-pci. The recommended way is to run
# kubevirt's kubevirtci kind-1.35-vfio-gpu provider script
# 'setup-host-vfio-pci.sh' first (which builds + loads the fake-iommu
# / fake-pci kernel modules and binds N synthetic PCI devices to
# vfio-pci). Real hardware in vfio mode also works.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

set -e
set -o pipefail

: "${KIND_CLUSTER_NAME:=kind-vfio-gpu}"
: "${KIND_NODE_IMAGE:=kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661}"
: "${KIND_CONFIG:=${CURRENT_DIR}/kind-cluster-config.yaml}"

: "${KUBEVIRT_CHANNEL:=nightly}"

: "${DRA_NAMESPACE:=dra-example-driver-vfio}"
: "${DRA_RELEASE_NAME:=dra-example-driver-vfio}"
: "${RUN_DRA_INSTALL:=true}"

# Container runtime kind talks to. Auto-detected in preflight if unset
# (prefers docker, falls back to podman). When podman is selected, the
# preflight also exports KIND_EXPERIMENTAL_PROVIDER=podman so 'kind'
# uses it instead of docker.
: "${CONTAINER_TOOL:=}"

: "${UPSTREAM_HOST_SETUP_HINT:=https://github.com/kubevirt/kubevirt/tree/main/kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu}"

# --- logging --------------------------------------------------------------

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
log_info() { printf "${GREEN}[INFO]${NC} %s\n" "$*"; }
log_warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
log_err()  { printf "${RED}[ERROR]${NC} %s\n" "$*" >&2; }
die()      { log_err "$*"; exit 1; }

preflight() {
    log_info "Pre-flight checks"

    if [[ "$(uname -s)" != "Linux" ]]; then
        die "Host is $(uname -s). The vfio-gpu demo needs a Linux host" \
            "kernel with vfio-pci-bound devices."
    fi

    # Auto-detect container runtime if not explicitly set. Prefer docker
    # if both are installed, since it's the kind default (no extra env
    # plumbing required); fall back to podman.
    if [[ -z "${CONTAINER_TOOL}" ]]; then
        if command -v docker >/dev/null; then
            CONTAINER_TOOL=docker
        elif command -v podman >/dev/null; then
            CONTAINER_TOOL=podman
        else
            die "neither docker nor podman found in PATH"
        fi
        log_info "  detected CONTAINER_TOOL=${CONTAINER_TOOL}"
    else
        command -v "${CONTAINER_TOOL}" >/dev/null \
            || die "CONTAINER_TOOL=${CONTAINER_TOOL} but '${CONTAINER_TOOL}' not found in PATH"
    fi
    export CONTAINER_TOOL

    # kind defaults to docker; the podman backend is opt-in via env.
    if [[ "${CONTAINER_TOOL}" == "podman" ]]; then
        export KIND_EXPERIMENTAL_PROVIDER=podman
        log_info "  kind backend: podman (KIND_EXPERIMENTAL_PROVIDER=podman)"
    fi

    local tool
    for tool in kind kubectl helm curl sudo; do
        command -v "${tool}" >/dev/null || die "${tool} not found in PATH"
    done

    [[ -f "${KIND_CONFIG}" ]] || die "kind config not found at ${KIND_CONFIG}"

    if ! sudo -n true 2>/dev/null; then
        log_info "Priming sudo credentials (you'll be prompted once)"
        sudo -v || die "sudo authentication failed"
    fi
}

verify_vfio_setup() {
    log_info "Checking for vfio-pci bindings on the host"

    local devs=()
    local entry name
    shopt -s nullglob
    for entry in /sys/bus/pci/drivers/vfio-pci/[0-9a-fA-F]*:*; do
        name="${entry##*/}"
        # BDF format: 0000:00:00.0 - skip non-device entries like 'bind' / 'unbind' / 'module'
        [[ "${name}" =~ ^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$ ]] || continue
        devs+=("${name}")
    done
    shopt -u nullglob

    if [[ ${#devs[@]} -eq 0 ]]; then
        log_err "No PCI devices bound to vfio-pci on this host."
        log_err ""
        log_err "This script only creates the cluster on top of a host that is"
        log_err "already vfio-pci-prepared. Set up the host first, then re-run."
        log_err ""
        log_err "Synthetic PCI devices (recommended for testing) are provided"
        log_err "by kubevirt's kubevirtci kind-1.35-vfio-gpu provider:"
        log_err ""
        log_err "    ${UPSTREAM_HOST_SETUP_HINT}"
        log_err ""
        log_err "Get a copy of that directory (clone kubevirt, sparse-checkout,"
        log_err "or just download the files) and run:"
        log_err ""
        log_err "    sudo FAKE_PCI_DEVICES=8 bash setup-host-vfio-pci.sh"
        log_err ""
        log_err "Then verify:"
        log_err ""
        log_err "    ls /sys/bus/pci/drivers/vfio-pci/   # should list BDFs"
        exit 1
    fi

    log_info "  found ${#devs[@]} vfio-pci device(s):"
    printf '    %s\n' "${devs[@]}"
}

cluster_up() {
    if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        log_info "kind cluster '${KIND_CLUSTER_NAME}' already exists; reusing"
    else
        log_info "Pre-pulling kind node image: ${KIND_NODE_IMAGE}"
        "${CONTAINER_TOOL}" pull -q "${KIND_NODE_IMAGE}" >/dev/null \
            || log_warn "Could not pre-pull ${KIND_NODE_IMAGE}; kind will pull on create"

        log_info "Creating kind cluster '${KIND_CLUSTER_NAME}'"
        kind create cluster \
            --name "${KIND_CLUSTER_NAME}" \
            --image "${KIND_NODE_IMAGE}" \
            --config "${KIND_CONFIG}" \
            --retain \
            --wait 5m \
            || {
                log_err "kind create failed; dumping logs to /tmp/kind-vfio-gpu-logs"
                kind export logs --name "${KIND_CLUSTER_NAME}" /tmp/kind-vfio-gpu-logs 2>/dev/null || true
                kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
                die "kind cluster create failed (see /tmp/kind-vfio-gpu-logs)"
            }
    fi

    log_info "Exporting kubeconfig for kind cluster ${KIND_CLUSTER_NAME}"
    kind export kubeconfig --name "${KIND_CLUSTER_NAME}"
    kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null
    kubectl cluster-info >/dev/null \
        || die "API server for kind-${KIND_CLUSTER_NAME} is unreachable"
}

post_create_config() {
    log_info "Configuring nodes (sysfs rw + /dev/vfio/vfio perms)"

    local node
    while IFS= read -r node; do
        [[ -z "${node}" ]] && continue
        log_info "  ${node}"
        "${CONTAINER_TOOL}" exec "${node}" mount -o remount,rw /sys

        if "${CONTAINER_TOOL}" exec "${node}" test -e /dev/vfio/vfio; then
            "${CONTAINER_TOOL}" exec "${node}" chmod 666 /dev/vfio/vfio
        else
            log_warn "    /dev/vfio/vfio not present in ${node}" \
                "- check that vfio-pci is loaded on the host"
        fi

        local discovered
        discovered=$("${CONTAINER_TOOL}" exec "${node}" \
            sh -c 'ls /sys/bus/pci/drivers/vfio-pci/ 2>/dev/null' \
            | grep -E '^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$' \
            || true)
        if [[ -n "${discovered}" ]]; then
            echo "${discovered}" | sed 's/^/      /'
        else
            log_warn "    no vfio-pci devices visible from inside ${node}"
        fi
    done < <(kind get nodes --name "${KIND_CLUSTER_NAME}")
}

install_kubevirt() {
    log_info "Installing KubeVirt"

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
            kv_channel="nightly"
            kv_version="${KUBEVIRT_CHANNEL}"
            kv_op_url="${nightly_root}/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="${nightly_root}/${kv_version}/kubevirt-cr.yaml"
            ;;
        v*)
            kv_channel="release"
            kv_version="${KUBEVIRT_CHANNEL}"
            kv_op_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-operator.yaml"
            kv_cr_url="https://github.com/kubevirt/kubevirt/releases/download/${kv_version}/kubevirt-cr.yaml"
            ;;
        *)
            die "Unrecognized KUBEVIRT_CHANNEL='${KUBEVIRT_CHANNEL}'. Expected" \
                "'stable', 'nightly', YYYYMMDD, or v<x.y.z>."
            ;;
    esac
    log_info "  channel: ${kv_channel}"
    log_info "  version: ${kv_version}"

    if ! curl -fsI "${kv_op_url}" >/dev/null; then
        die "KubeVirt operator manifest not reachable: ${kv_op_url}" \
            "(check KUBEVIRT_CHANNEL='${KUBEVIRT_CHANNEL}')"
    fi

    kubectl apply -f "${kv_op_url}"
    kubectl apply -f "${kv_cr_url}"

    log_info "Waiting for KubeVirt to be Available (5-10 minutes)"
    kubectl -n kubevirt wait kv kubevirt --for condition=Available --timeout=600s

    log_info "Enabling DRA + HostDevices feature gates on the KubeVirt CR"
    kubectl patch kubevirt kubevirt -n kubevirt --type=merge -p \
        '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["GPUsWithDRA","HostDevicesWithDRA","HostDevices"]}}}}'
}

# --- main -----------------------------------------------------------------

preflight
verify_vfio_setup
cluster_up
post_create_config
install_kubevirt

cat <<EOF

${GREEN}Cluster ready.${NC}

Try the demo VMI:

    kubectl apply -f ${CURRENT_DIR}/vfio-gpu-test.yaml
    kubectl -n vfio-vm-test get vmi,pod,resourceclaims

Tear down with:

    bash ${CURRENT_DIR}/delete-cluster.sh

EOF
