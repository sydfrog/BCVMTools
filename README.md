# BCVMTools

A fast, single-file VMware inventory exporter modelled on RVTools. Written in Go: one static
executable, no runtime, no install. Works against vCenter or a standalone ESXi host.

## Usage

```bash
bcvmtools -s vcenter.example.com -u administrator@vsphere.local          # prompts for password
bcvmtools -s vcenter.example.com -u svc-ro -p 'secret' -o inventory.xlsx
bcvmtools -s esxi01 -u root -f csv -o ./out                              # one CSV per tab
```

Env vars `VI_SERVER`, `VI_USER`, `VI_PASSWORD` are honoured. Certificate checks are skipped by
default (`-insecure=false` to enforce). A read-only vCenter role is enough.

## Tabs

vInfo, vCPU, vMemory, vDisk, vPartition, vNetwork, vSnapshot, vTools, vCD, vHost, vCluster, vRP,
vDatastore, vSwitch, vPort, vSC_VMK, vNIC, vHBA, dvSwitch, dvPort, vLicense, vSource, vHealth.

Not (yet) replicated from RVTools: vUSB, vFileInfo, vMultiPath, vSC_VMK extras, performance history.

## Why it is fast

* One bulk property-collector retrieval per object type, all issued concurrently.
* VMs are fetched in parallel 200-VM shards.
* Only needed properties are requested; the xlsx is written with a streaming writer.

## Build

```bash
make            # linux/windows/darwin binaries in dist/
```
