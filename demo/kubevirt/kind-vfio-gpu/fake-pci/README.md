# Fake PCI Kernel Emulation

This kernel module exposes **example DRA PCI devices** under
`/sys/bus/pci/devices/` so that the KubeVirt DRA `pciBusID` path can be
exercised in the `kind-vfio-gpu` cluster **without any real PCI hardware**.

The devices use intentionally unassigned vendor/device IDs (default
`0xe1a5:0xd0c5`, subsystem `0xe1a5:0xd0c5`) and live in their own private
PCI domain (default `0xfaca`) so they cannot collide with the real PCI
hierarchy at domain `0000`. Override the IDs via module parameters if a
test needs to mimic a specific real-world vendor/SKU pair.

A companion mdev driver may be loaded alongside this module for full
vGPU-style attach tests; the two are independent.

## What you get

After loading the module with defaults, `lspci -D -nn -d e1a5:` shows:

```
faca:00:00.0 3D controller [0302]: Device e1a5:d0c5
faca:00:01.0 3D controller [0302]: Device e1a5:d0c5
faca:00:02.0 3D controller [0302]: Device e1a5:d0c5
faca:00:03.0 3D controller [0302]: Device e1a5:d0c5
```

`lspci` reports them as "Device <id>" (with no vendor name) because
`0xe1a5` is unassigned in the public `pci.ids` database — honest for an
example module.

Each device exposes the full set of standard PCI sysfs attributes
(`vendor`, `device`, `class`, `subsystem_vendor`, `subsystem_device`,
`config`, `irq`, etc.) so userspace tooling treats them as ordinary PCI
devices.

## Scope and limitations

Capabilities depend on whether the companion `fake-iommu` module is also
loaded. `setup-fake-pci-host.sh` loads both by default (`FAKE_IOMMU=true`).

| Capability | This module alone | + fake-iommu |
|---|---|---|
| `/sys/bus/pci/devices/<bdf>/` entries | yes | yes |
| `lspci -D` listing | yes | yes |
| DRA driver discovery (scan + ResourceSlice publish) | yes | yes |
| KEP-5304 metadata round-trip via `GetPCIAddressForClaim` | yes | yes |
| Building libvirt domXML in virt-launcher | yes | yes |
| `iommu_group` symlink under `/sys/bus/pci/devices/<bdf>/` | no | **yes** |
| `vfio-pci` binding to the fake devices | no | **yes** |
| `/dev/vfio/<group>` device files | no | **yes** |
| virt-handler's pre-start hook succeeds | no | **yes** |
| **VMI reaches `Running`** | **no** | **yes** |
| Guest sees a non-crashing PCI device | no | requires fake BAR backing (not implemented) |
| Guest has working hardware behind the BARs | no | no (no real device) |
| Hot-plug emulation via `hotplug_control` | yes | yes |

For "VMI reaches Running" tests, load both modules (the default).
For pure discovery / claim / metadata tests, `FAKE_IOMMU=false` is enough.
For full VM-attach with a guest that actually uses the device, use an mdev
driver — mdev provides its own emulated VFIO interface.

## Requirements

- Linux kernel **5.16+** built with `CONFIG_PCI_DOMAINS=y` and **one of**:
  - `CONFIG_PCI_DOMAINS_GENERIC=y` — the generic path, used by Fedora 39+,
    RHEL 9, and arm64 / non-x86 distro kernels. The bridge's `domain_nr`
    field is honored directly.
  - An **x86 build** (Ubuntu's default `x86_64` kernels are GENERIC=n).
    The module attaches a `struct pci_sysdata` to the bridge so the
    architecture's `pci_domain_nr()` reader picks up our private domain.

  Verify with `zgrep PCI_DOMAINS /proc/config.gz` or
  `grep PCI_DOMAINS /boot/config-$(uname -r)`. Builds for non-x86 with
  GENERIC=n hard-error in `compat.h`.

- Kernel headers matching the running kernel
- `make`, `gcc`, root privileges to insmod

## Build

```bash
cd demo/clusters/kind-vfio-gpu/fake-pci

# Using a container that matches the kernel of CI hosts:
docker run --rm \
  -v $(pwd):/src:Z \
  quay.io/kubevirtci/bootstrap:v20251218-e7a7fc9 \
  bash -c 'dnf install -y kernel-devel >/dev/null 2>&1 && cd /src && \
           make KDIR=/usr/src/kernels/$(ls /usr/src/kernels/ | head -1) modules'

# Or just on the host:
make
```

## Load / unload

```bash
cd demo/clusters/kind-vfio-gpu

# Load with defaults (4 fake devices in domain 0xfaca):
sudo ./setup-fake-pci-host.sh setup

# Inspect:
sudo ./setup-fake-pci-host.sh status
lspci -D -nn -d e1a5:

# Unload:
sudo ./setup-fake-pci-host.sh cleanup
```

### Environment variables (passed as module parameters)

| Variable | Default | Module param |
|---|---|---|
| `FAKE_PCI_DEVICES` | `4` | `num_devices` (1..32) |
| `FAKE_PCI_DOMAIN` | `0xfaca` | `pci_domain` |
| `FAKE_PCI_VENDOR_ID` | `0xe1a5` | `vendor_id` |
| `FAKE_PCI_DEVICE_ID` | `0xd0c5` | `device_id` |
| `FAKE_PCI_SUBSYS_ID` | `0xd0c5` | `subsys_id` |

Examples:

```bash
# 8 fake devices:
FAKE_PCI_DEVICES=8 sudo ./setup-fake-pci-host.sh setup

# 2 devices in a different domain (avoid collision if 0xfaca is taken),
# using a different synthetic device ID:
FAKE_PCI_DEVICES=2 FAKE_PCI_DEVICE_ID=0xf00d FAKE_PCI_DOMAIN=0xfada \
  sudo ./setup-fake-pci-host.sh setup

# Pretend to be a specific real-world SKU (e.g. to exercise a driver that
# filters on vendor ID). Make sure no real device with the same IDs is
# present on the host:
FAKE_PCI_VENDOR_ID=0x10de FAKE_PCI_DEVICE_ID=0x1eb8 \
  sudo ./setup-fake-pci-host.sh setup
```

## Hot-plug emulation

```bash
sudo ./setup-fake-pci-host.sh hide   # tear down the synthetic bus
sudo ./setup-fake-pci-host.sh show   # re-create it

# Or directly:
echo hide > /sys/class/fake_pci/control/hotplug_control
echo show > /sys/class/fake_pci/control/hotplug_control
cat       /sys/class/fake_pci/control/hotplug_control
```

## How it works

1. The module allocates a `pci_host_bridge` via `pci_alloc_host_bridge()`,
   sets its `ops` to the module's `pci_ops.read` / `pci_ops.write`
   callbacks, and assigns `domain_nr = pci_domain`.
2. `pci_host_probe()` triggers a normal PCI scan on this new bridge. Our
   read callback synthesizes the configured vendor/device/class config
   space for slots `0 .. num_devices-1` and returns `0xffff` (no device)
   for all other slots and functions. The kernel creates `pci_dev`
   objects and the standard sysfs entries.
3. BARs are reported as 0. When the PCI core probes BAR sizes (writing
   `0xFFFFFFFF` then reading back), our write callback stores 0 and the
   readback yields 0, which the core interprets as "BAR not implemented".
   No iomem windows are allocated.
4. The hot-plug control device under `/sys/class/fake_pci/control/`
   exposes a `hotplug_control` file that tears down (`pci_stop_root_bus`
   + `pci_remove_root_bus`) or re-creates the synthetic bridge.

## How a DRA driver consumes this

A DRA driver running in the kind node:

1. Scans `/sys/bus/pci/drivers/vfio-pci/` (devices already bound to
   `vfio-pci` by the operator) and reads `vendor` / `device` / `class`
   for each BDF from `/sys/bus/pci/devices/<bdf>/`. With this module
   loaded, the entries on bus `faca:` advertise `vendor=0xe1a5`,
   `device=0xd0c5`.
2. Publishes a `ResourceSlice` advertising each fake BDF as a DRA device
   with attributes such as
   `resource.kubernetes.io/pciBusID = "faca:00:00.0"`,
   `VendorID = "e1a5"`, `DeviceID = "d0c5"`, and `iommuGroup = <N>`.
3. On `NodePrepareResources`, the upstream kubeletplugin framework writes
   the KEP-5304 metadata file at
   `/var/run/kubernetes.io/dra-device-attributes/<claim>/<request>/metadata.json`
   with `Attributes["resource.kubernetes.io/pciBusID"].StringValue = <bdf>`.
4. KubeVirt's `virt-launcher` reads that file via
   `pkg/dra/utils.go::GetPCIAddressForClaim` and builds the libvirt
   `<hostdev type='pci'>` block via
   `pkg/virt-launcher/virtwrap/device/hostdevice/dra/gpu_hostdev.go`.

The VM start will succeed up to the QEMU `vfio-pci` attach step. Whether
the guest then sees a working device depends on whether a companion mdev
driver is providing real DMA backing — this module alone has none.

## Files

| File | License | Notes |
|---|---|---|
| `fake-pci.c` | GPL-2.0-only | Kernel module |
| `compat.h` | GPL-2.0-only | Kernel version shims |
| `Makefile` | GPL-2.0-only | kbuild |
| `dkms.conf` | GPL-2.0-only | DKMS install descriptor |
| `.clangd` | — | Silences IDE diagnostics about missing `linux/*.h` headers |
| `README.md` | Apache-2.0 | This file |
| `../fake-iommu/` | GPL-2.0-only | Companion no-op IOMMU; required for `vfio-pci` binding |
| `../setup-fake-pci-host.sh` | Apache-2.0 | Host-level helper (loads both modules in the right order) |

## References
- Linux PCI host bridge API: <https://docs.kernel.org/PCI/index.html>
