# KubeVirt + DRA vfio-gpu demo (kind)

This directory brings up a kind cluster wired for the `vfio-gpu` profile of
the example DRA driver, using **synthetic** PCI devices created by the
`fake-iommu` and `fake-pci` kernel modules. KubeVirt is installed on top
with the `GPUsWithDRA` / `HostDevicesWithDRA` feature gates enabled, so
DRA-allocated devices can be claimed by a VMI.

> **Linux host only.** `fake-iommu.ko` and `fake-pci.ko` are kernel
> modules that must be `insmod`'d into the live host kernel. macOS
> Docker Desktop and remote-Docker setups don't expose the host kernel,
> so they can't run this demo.

## Contents

| File / dir                  | Purpose                                                          |
| --------------------------- | ---------------------------------------------------------------- |
| `fake-iommu/`               | Tiny kernel module that exposes a fake IOMMU group                |
| `fake-pci/`                 | Kernel module that publishes 4 synthetic PCI devices on bus `faca` |
| `setup-fake-pci-host.sh`    | Loads the modules and binds the synthetic devices to `vfio-pci`   |
| `kind-cluster-config.yaml`  | kind config (api-server feature gates, mounts, etc.)              |
| `config-vfio-cluster.sh`    | Post-create per-node tweaks (`/sys` remount, `/dev/vfio` perms)   |
| `create-cluster.sh`         | One-shot: pre-flight → build modules → host setup → kind → KubeVirt |
| `delete-cluster.sh`         | Tear down kind + unbind modules                                   |

## Prerequisites

* Linux (tested on Ubuntu 22.04, kernel 6.x)
* Kernel headers for your running kernel:
  * Debian/Ubuntu: `sudo apt-get install linux-headers-$(uname -r)`
  * Fedora/RHEL: `sudo dnf install kernel-devel-$(uname -r)`
* `gcc`, `make`, `kind`, `docker`, `kubectl`, `helm`
* `sudo` access (script primes credentials once)

---

## One-shot: end-to-end with `create-cluster.sh`

For most cases, this is all you need:

```bash
./demo/kubevirt/kind-vfio-gpu/create-cluster.sh
```

That runs steps 1–6 below:

1. **Pre-flight** (host OS, kernel headers, tools, sudo).
2. **Build the kernel modules** (`fake-iommu.ko`, `fake-pci.ko`).
3. **Host setup**: load modules and bind the 4 synthetic devices to
   `vfio-pci` (so `/dev/vfio/<group>` shows up).
4. **kind create cluster** against `kind-cluster-config.yaml`,
   then re-export the kubeconfig and verify the API server is reachable.
5. **Per-node configuration** (`config-vfio-cluster.sh`).
6. **Install KubeVirt** and enable DRA feature gates on the KubeVirt CR.
### `KUBEVIRT_CHANNEL` (which KubeVirt to install)

The driver consumes the **KEP-5304** device metadata layout. virt-launcher
support for that layout for resource-claim *templates* was added in
[`kubevirt/kubevirt#17028`](https://github.com/kubevirt/kubevirt/pull/17028),
which merged to `main` on Apr 26, 2026 and was cherry-picked to
`release-1.8` as `#17631`. **It is not in the current stable `v1.8.2` tag.** So
the default channel here is `nightly` (latest `main` build):

`KUBEVIRT_CHANNEL` accepts four forms; the script dispatches based on
the format:

| `KUBEVIRT_CHANNEL=`         | Source                                          | Example URL                                                                              |
| --------------------------- | ----------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `nightly` (default)         | latest nightly main build (resolves at runtime) | `.../nightly/.../<latest>/kubevirt-operator.yaml`                                        |
| `<YYYYMMDD>` (e.g. `20260427`) | specific nightly date                        | `.../nightly/.../20260427/kubevirt-operator.yaml`                                        |
| `stable`                    | latest tagged release (via `stable.txt`)         | `github.com/.../releases/download/v1.8.2/kubevirt-operator.yaml`                          |
| `v<x.y.z>` (e.g. `v1.8.3`)  | specific release tag                            | `github.com/.../releases/download/v1.8.3/kubevirt-operator.yaml`                          |

```bash
# Default - latest nightly main (always has #17028 + later changes)
./demo/kubevirt/kind-vfio-gpu/create-cluster.sh

# Pin to a specific nightly date (first nightly that contains #17028)
KUBEVIRT_CHANNEL=20260427 ./demo/kubevirt/kind-vfio-gpu/create-cluster.sh

# Last tagged release (v1.8.2 today - will MISS #17028 until v1.8.3 ships)
KUBEVIRT_CHANNEL=stable   ./demo/kubevirt/kind-vfio-gpu/create-cluster.sh

# Pin to a specific release tag (once v1.8.3 ships with the cherry-pick)
KUBEVIRT_CHANNEL=v1.8.3   ./demo/kubevirt/kind-vfio-gpu/create-cluster.sh
```

The script prints both the resolved channel and version up front, e.g.:

```
[INFO]   channel: nightly
[INFO]   version: 20260520
```

---
## Deploy the driver

`create-cluster.sh` does **not** build or load the dra-example-driver
image. Do these three steps after the cluster is up.

### A. Build the driver image (host docker daemon)

```bash
./demo/scripts/build-driver-image.sh

DRIVER_TAG=$(grep '^appVersion:' deployments/helm/dra-example-driver/Chart.yaml \
              | awk '{print $2}' | tr -d '"')
DRIVER_IMG="registry.k8s.io/dra-example-driver/dra-example-driver:${DRIVER_TAG}"

docker images "${DRIVER_IMG}"
```

### B. Load the image into kind nodes (manual, reliable path)

`load-driver-image-into-kind.sh` uses `kind load image-archive`, which
on certain kind / containerd version combinations silently keeps an old
image. Use this `ctr import` path instead. **Note: kind nodes mount
`/tmp` as tmpfs, so we copy to `/root` instead of `/tmp`.**

```bash
docker save -o /tmp/driver.tar "${DRIVER_IMG}"

for node in vfio-gpu-cluster-control-plane; do
  docker exec "${node}" crictl rmi "${DRIVER_IMG}" 2>/dev/null || true
  docker cp /tmp/driver.tar "${node}:/root/driver.tar"
  docker exec "${node}" ctr -n=k8s.io image import /root/driver.tar
  docker exec "${node}" rm -f /root/driver.tar
done

rm -f /tmp/driver.tar
```
### C. Install the Helm chart (vfio-gpu profile)

`--set` flags have proved unreliable for this chart — always pass values
via a file:

```bash
helm upgrade --install \
    --create-namespace \
    --namespace dra-example-driver-vfio \
    --set deviceProfile=vfio-gpu \
    --set kubeletPlugin.enableDeviceMetadata=true \
    --set driverName=vfio-gpu.example.com \
    dra-example-driver-vfio \
    deployments/helm/dra-example-driver
```

`enableDeviceMetadata: true` is required for KubeVirt — it tells the
kubeletplugin framework to write the KEP-5304 metadata file that
virt-launcher reads on VM startup.

### D. Verify the driver is running the vfio-gpu profile

```bash
NS=dra-example-driver-vfio

kubectl -n $NS rollout status ds dra-example-driver-vfio-kubeletplugin --timeout=120s

# Env shows DEVICE_PROFILE=vfio-gpu and DRIVER_NAME=vfio-gpu.example.com
kubectl -n $NS get ds dra-example-driver-vfio-kubeletplugin \
    -o jsonpath='{range .spec.template.spec.containers[?(@.name=="plugin")].env[*]}{.name}={.value}{"\n"}{end}' \
    | grep -E 'DEVICE_PROFILE|DRIVER_NAME|PCI_'

# Slices should be vfio-gpu.example.com with the BDFs of the synthetic devices
kubectl get resourceslices \
    -o custom-columns='NAME:.metadata.name,DRIVER:.spec.driver,NODE:.spec.nodeName'

kubectl get deviceclasses
```

---

## Run the VMI demo

```bash
kubectl apply -f demo/kubevirt/kind-vfio-gpu/vfio-gpu-test.yaml
kubectl get vmi,pod -n vfio-vm-test
kubectl describe vmi -n vfio-vm-test
```

A successful run shows the VMI in `Scheduled`/`Running` and a
ResourceClaim bound to one of the synthetic PCI BDFs.

---

## Tear down

```bash
./demo/kubevirt/kind-vfio-gpu/delete-cluster.sh
# also unloads the modules and unbinds the synthetic devices
```

---
