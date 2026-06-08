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

preflight() {
    echo "Pre-flight checks"

    if [[ "$(uname -s)" != "Linux" ]]; then
        echo "Host is $(uname -s). The vfio-gpu demo needs a Linux host" \
            "kernel with vfio-pci-bound devices."
        exit 1
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
            echo "neither docker nor podman found in PATH"
            exit 1
        fi
        echo "detected CONTAINER_TOOL=${CONTAINER_TOOL}"
    else
        command -v "${CONTAINER_TOOL}" >/dev/null \
            || echo "CONTAINER_TOOL=${CONTAINER_TOOL} but '${CONTAINER_TOOL}' not found in PATH" \
            exit 1
    fi
    export CONTAINER_TOOL

    # kind defaults to docker; the podman backend is opt-in via env.
    if [[ "${CONTAINER_TOOL}" == "podman" ]]; then
        export KIND_EXPERIMENTAL_PROVIDER=podman
        echo "kind backend: podman (KIND_EXPERIMENTAL_PROVIDER=podman)"
    fi

    local tool
    for tool in kind kubectl helm curl sudo; do
        command -v "${tool}" >/dev/null || echo "${tool} not found in PATH" \
            exit 1
    done

    [[ -f "${KIND_CONFIG}" ]] || echo "kind config not found at ${KIND_CONFIG}" \
        exit 1

    if ! sudo -n true 2>/dev/null; then
        echo "Priming sudo credentials (you'll be prompted once)"
        sudo -v || echo "sudo authentication failed" \
            exit 1
    fi
}

verify_vfio_setup() {
    echo "Checking for vfio-pci bindings on the host"

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
        echo "No PCI devices bound to vfio-pci on this host."
        echo ""
        echo "This script only creates the cluster on top of a host that is"
        echo "already vfio-pci-prepared. Set up the host first, then re-run."
        echo ""
        echo "Synthetic PCI devices (recommended for testing) are provided"
        echo "by kubevirt's kubevirtci kind-1.35-vfio-gpu provider:"
        echo ""
        echo "    ${UPSTREAM_HOST_SETUP_HINT}"
        echo ""
        echo "Get a copy of that directory (clone kubevirt, sparse-checkout,"
        echo "or just download the files) and run:"
        echo ""
        echo "    sudo FAKE_PCI_DEVICES=8 bash setup-host-vfio-pci.sh"
        echo ""
        echo "Then verify:"
        echo ""
        echo "    ls /sys/bus/pci/drivers/vfio-pci/   # should list BDFs"
        exit 1
    fi

    echo "  found ${#devs[@]} vfio-pci device(s):"
    printf '    %s\n' "${devs[@]}"
}

cluster_up() {
    if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        echo "kind cluster '${KIND_CLUSTER_NAME}' already exists; reusing"
    else
        echo "Pre-pulling kind node image: ${KIND_NODE_IMAGE}"
        "${CONTAINER_TOOL}" pull -q "${KIND_NODE_IMAGE}" >/dev/null \
            || echo "Could not pre-pull ${KIND_NODE_IMAGE}; kind will pull on create"

        echo "Creating kind cluster '${KIND_CLUSTER_NAME}'"
        kind create cluster \
            --name "${KIND_CLUSTER_NAME}" \
            --image "${KIND_NODE_IMAGE}" \
            --config "${KIND_CONFIG}" \
            --retain \
            --wait 5m \
            || {
                echo "kind create failed; dumping logs to /tmp/kind-vfio-gpu-logs"
                kind export logs --name "${KIND_CLUSTER_NAME}" /tmp/kind-vfio-gpu-logs 2>/dev/null || true
                kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
                echo "kind cluster create failed (see /tmp/kind-vfio-gpu-logs)"
                exit 1
            }
    fi

    echo "Exporting kubeconfig for kind cluster ${KIND_CLUSTER_NAME}"
    kind export kubeconfig --name "${KIND_CLUSTER_NAME}"
    kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null
    kubectl cluster-info >/dev/null \
        || echo "API server for kind-${KIND_CLUSTER_NAME} is unreachable" \
        exit 1
}

post_create_config() {
    echo "Configuring nodes (sysfs rw + /dev/vfio/vfio perms)"

    local node
    while IFS= read -r node; do
        [[ -z "${node}" ]] && continue
        echo "  ${node}"
        "${CONTAINER_TOOL}" exec "${node}" mount -o remount,rw /sys

        if "${CONTAINER_TOOL}" exec "${node}" test -e /dev/vfio/vfio; then
            "${CONTAINER_TOOL}" exec "${node}" chmod 666 /dev/vfio/vfio
        else
            echo "    /dev/vfio/vfio not present in ${node}" \
                "- check that vfio-pci is loaded on the host" \
                exit 1
        fi

        local discovered
        discovered=$("${CONTAINER_TOOL}" exec "${node}" \
            sh -c 'ls /sys/bus/pci/drivers/vfio-pci/ 2>/dev/null' \
            | grep -E '^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$' \
            || true)
        if [[ -n "${discovered}" ]]; then
            echo "${discovered}" | sed 's/^/      /'
        else
            echo "    no vfio-pci devices visible from inside ${node}"
        fi
    done < <(kind get nodes --name "${KIND_CLUSTER_NAME}")
}

# --- main -----------------------------------------------------------------

preflight
verify_vfio_setup
cluster_up
post_create_config

cat <<EOF

${GREEN}Cluster ready.${NC}

Try the demo VMI:

    kubectl apply -f /demo/clusters/kind-vfio-gpu/vfio-gpu-test.yaml
    kubectl -n vfio-vm-test get vmi,pod,resourceclaims

Tear down with:

    bash ${CURRENT_DIR}/delete-cluster.sh

EOF
