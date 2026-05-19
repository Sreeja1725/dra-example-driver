# Cluster lifecycle

This directory holds platform-specific scripts and documentation for bringing
up Kubernetes clusters with Dynamic Resource Allocation (DRA) enabled, for
use with the demo in this repository.

Each subdirectory is named for its platform. Where applicable, scripts follow a
common layout:

- `create-cluster.sh` — create a cluster configured for the demo
- `delete-cluster.sh` — delete that cluster

Platforms may add other scripts or notes next to these entrypoints as needed.

## Available platforms

| Path | Purpose |
|---|---|
| [`kind/`](kind/) | Plain kind cluster for the default `gpu` (mock devices) DRA profile. Use this for the README quickstart and the `demo/gpu-test*.yaml` fixtures. |
| [`kind-vfio-gpu/`](kind-vfio-gpu/) | kind cluster wired for the `vfio-gpu` profile. Builds and loads the `fake-iommu` + `fake-pci` kernel modules on the host so the driver can advertise synthetic vfio-pci devices end-to-end (KEP-5304 metadata + `/dev/vfio` CDI injection). Linux host with kernel headers required. |
