# KubeVirt + DRA vfio-gpu demo (kind)

Create a kind cluster on top of a host that **already** has PCI devices
bound to `vfio-pci`, then install KubeVirt and the dra-example-driver
helm chart with the `vfio-gpu` profile so a VMI can claim DRA-allocated
PCI passthrough devices.

This demo has a clean split of responsibility:

| Layer  | Owner                          | What happens                                                       |
| -----  | ------------------------------ | ------------------------------------------------------------------ |
| Host   | **You** (via kubevirt scripts) | Load `fake-iommu` / `fake-pci`, bind synthetic devices to vfio-pci |
| Cluster | This script                   | `kind create cluster`, install KubeVirt + DRA, install our chart   |

`create-cluster.sh` does **not** download, clone, or fetch anything from
the kubevirt repo. It just verifies that vfio-pci has bindings on the host
and refuses to continue if it doesn't, pointing you at the kubevirt
prerequisite.

> **Linux host only.** vfio-pci binding requires custom kernel modules
> (`fake-iommu.ko`, `fake-pci.ko`, or real-hardware drivers). macOS Docker
> Desktop and remote-Docker setups cannot do this.

## Contents

| File                       | Purpose                                                                       |
| -------------------------- | ----------------------------------------------------------------------------- |
| `create-cluster.sh`        | Cluster bring-up wrapper (kind + KubeVirt + helm)                             |
| `delete-cluster.sh`        | Tear down (`kind delete cluster` + kubeconfig cleanup)                        |
| `kind-cluster-config.yaml` | Vendored kind config (DRA gates, CDI, CPUManager, /sys/bus/pci extraMounts)   |
| `vfio-gpu-test.yaml`       | VMI + ResourceClaimTemplate that exercises the vfio-gpu profile               |

## Prerequisites

### Host has devices bound to vfio-pci

This script will not create the kind cluster unless
`/sys/bus/pci/drivers/vfio-pci/` has at least one BDF entry.

The recommended way to get synthetic devices is **kubevirt's kubevirtci
`kind-1.35-vfio-gpu` provider**, which ships the `fake-iommu` / `fake-pci`
kernel modules and the host setup wrapper:

> [`kubevirt/kubevirt:kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu`](https://github.com/kubevirt/kubevirt/tree/main/kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu)

Readme has all the steps

Obtain the provider directory however suits you — clone the kubevirt repo,
`git sparse-checkout` just that subtree, download the files directly via
the GitHub UI, copy them from a colleague, whatever. Then on the host:

```bash
# from inside the provider directory
sudo FAKE_PCI_DEVICES=8 bash setup-host-vfio-pci.sh
```

Verify before running this demo:

```bash
ls /sys/bus/pci/drivers/vfio-pci/   # expect N entries like faca:00:0X.0
```

Real hardware bound to vfio-pci works equally well — this demo doesn't
care whether the devices are synthetic or real, only that they exist.

## Quick start

Once the host has vfio-pci bindings:

Build the image for the example resource driver:
```bash
./demo/build-driver.sh
```

```bash
./demo/kubevirt/kind-vfio-gpu/create-cluster.sh
```

That runs:

1. Pre-flight (Linux, tools, sudo).
2. Verify vfio-pci has bindings — refuse with a clear pointer if not.
3. `kind create cluster --config kind-cluster-config.yaml` (the config
   bind-mounts `/sys/bus/pci` and `/sys/kernel/iommu_groups` into every
   node so the kubelet plugin can see the devices).
4. Per-node post-create: remount `/sys` rw, `chmod 666 /dev/vfio/vfio`,
   list the devices each node can see.
5. Install KubeVirt (nightly by default — must contain
   [kubevirt/kubevirt#17028](https://github.com/kubevirt/kubevirt/pull/17028))
   and enable `GPUsWithDRA` / `HostDevicesWithDRA` / `HostDevices`.

Install dra-example-driver:

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

Then: 

```bash
kubectl apply -f demo/kubevirt/kind-vfio-gpu/vfio-gpu-test.yaml
kubectl -n vfio-vm-test get vmi,pod,resourceclaims
```

Tear down (cluster only — host vfio state is left intact, symmetric with
how it was set up):

```bash
./demo/kubevirt/kind-vfio-gpu/delete-cluster.sh
```

To then unload the kernel modules on the host, run the kubevirt-side
cleanup wherever you put those scripts, e.g.:

```bash
sudo bash <path-to>/setup-fake-pci-host.sh cleanup
```

## Configuration knobs

All overridable via env vars before invoking `create-cluster.sh`.

| Variable             | Default                                                                         | Purpose                                                                  |
| -------------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| `KIND_CLUSTER_NAME`  | `kind-vfio-gpu`                                                                 | kind cluster name (also the kubeconfig context as `kind-<name>`).        |
| `KIND_NODE_IMAGE`    | `kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661` | Pinned kind node image (matches kubevirt's `kind-1.35-vfio-gpu`).        |
| `KIND_CONFIG`        | `<this dir>/kind-cluster-config.yaml`                                           | Path to the kind cluster config.                                         |
| `KUBEVIRT_CHANNEL`   | `nightly`                                                                       | KubeVirt install channel. See [below](#kubevirt_channel-values).         |
| `CONTAINER_TOOL`     | auto-detected (prefers `docker`, falls back to `podman`)                        | `docker` or `podman`. When set to `podman`, the script also exports `KIND_EXPERIMENTAL_PROVIDER=podman` so kind uses it. |
| `RUN_DRA_INSTALL`    | `true`                                                                          | Set `false` to skip the helm install.                                    |
| `DRA_NAMESPACE`      | `dra-example-driver-vfio`                                                       | Namespace for the helm release.                                          |
| `DRA_RELEASE_NAME`   | `dra-example-driver-vfio`                                                       | Helm release name.                                                       |

### `KUBEVIRT_CHANNEL` values

The KEP-5304 metadata-layout fix in virt-launcher
([kubevirt/kubevirt#17028](https://github.com/kubevirt/kubevirt/pull/17028))
merged to `main` on Apr 26, 2026 and was cherry-picked to `release-1.8`. It
is **not** in `v1.8.2`. Use a channel that contains the fix:

| `KUBEVIRT_CHANNEL=`            | Source                              |
| ------------------------------ | ----------------------------------- |
| `nightly` (default)            | Latest nightly main build           |
| `<YYYYMMDD>` (e.g. `20260427`) | Specific nightly date               |
| `stable`                       | Last tagged release (`stable.txt`)  |
| `v<x.y.z>` (e.g. `v1.8.3`)     | Specific release tag                |

## Verify the driver is healthy after install

```bash
NS=dra-example-driver-vfio

kubectl -n $NS rollout status ds dra-example-driver-vfio-kubeletplugin --timeout=120s

# Env must show vfio-gpu profile + vfio-gpu.example.com driver name
kubectl -n $NS get ds dra-example-driver-vfio-kubeletplugin \
    -o jsonpath='{range .spec.template.spec.containers[?(@.name=="plugin")].env[*]}{.name}={.value}{"\n"}{end}' \
    | grep -E 'DEVICE_PROFILE|DRIVER_NAME'

# Slices should be driver=vfio-gpu.example.com, one per BDF
kubectl get resourceslices \
    -o custom-columns='NAME:.metadata.name,DRIVER:.spec.driver,NODE:.spec.nodeName'

# DeviceClass
kubectl get deviceclasses vfio-gpu.example.com
```

## Troubleshooting

### `No PCI devices bound to vfio-pci on this host`
Run the host prerequisite (see [Prerequisites](#Host-has-devices-bound-to-vfio-pci))
first. After it completes:

```bash
ls /sys/bus/pci/drivers/vfio-pci/
```

should list at least one BDF.

### `no vfio-pci devices visible from inside <node>`
The host has bindings but the kind node doesn't see them. The
`kind-cluster-config.yaml` bind-mounts `/sys/bus/pci` and
`/sys/kernel/iommu_groups`; verify it wasn't replaced or that the kind
container can read those paths:

```bash
# substitute podman for docker if you're using podman
docker exec kind-vfio-gpu-control-plane ls /sys/bus/pci/drivers/vfio-pci/
```

### virt-handler crashloops with `Failed to create an inotify watcher`
Host inotify limits too low. See Prerequisites.

```bash
sudo sysctl -w fs.inotify.max_user_instances=8192
sudo sysctl -w fs.inotify.max_user_watches=524288
kubectl -n kubevirt rollout restart ds virt-handler
```

### `kubectl ...: connection refused`
Stale kubeconfig context. Re-export:

```bash
kind export kubeconfig --name kind-vfio-gpu
kubectl cluster-info
```

### ResourceSlices still show `driver: gpu.example.com`
The chart is installed but a previous release or stale driver pod is
serving them. Confirm:

```bash
helm get values -n dra-example-driver-vfio dra-example-driver-vfio
# Must show: deviceProfile: vfio-gpu
```

If those values look right, the pod is on an old image — see the project
README for the `docker save | ctr import` recipe for loading a freshly
built driver image into kind nodes.
