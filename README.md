# BCVMTools

A fast, single-file VMware inventory exporter modelled on RVTools. Written in Go: one static
executable, no runtime, no install. Works against vCenter or a standalone ESXi host.

## GUI (Windows)

Double-click `bcvmtools-windows-amd64.exe` (or run it with no arguments, or `-gui`) to open the
window: enter vCenter/ESXi, user and password, press **Collect**, browse every tab (with a live
filter box), then **Export Excel...** or **Export CSV folder...** once you are happy with the data.
Handy: **Enter** in the password box or **F5** collects, **Ctrl+F** jumps to search, **Ctrl+S** exports to
Excel. Server, user name, TLS choice and window size are remembered (never the password) in
`%AppData%\BCVMTools\settings.json`.

Sort by clicking a heading. Filter by right-clicking a heading, using its arrow, or the **Filter column...**
button; filtered columns turn amber. Run it with any CLI flag from a terminal and it behaves as the command-line tool below.

## Usage

```bash
bcvmtools -s vcenter.example.com -u administrator@vsphere.local          # prompts for password
bcvmtools -s vcenter.example.com -u svc-ro -p 'secret' -o inventory.xlsx
bcvmtools -s esxi01 -u root -f csv -o ./out                              # one CSV per tab
```

Env vars `VI_SERVER`, `VI_USER`, `VI_PASSWORD` are honoured. Certificate checks are skipped by
default (`-insecure=false` to enforce). A read-only vCenter role is enough.

## Several vCenters / VCF

Every row on every tab has a **vCenter** column (first column) naming the vCenter it came from, and a
**vCenters** tab (first tab) lists each connection: status, source, version, build, counts, time and any error.

* **GUI:** the main form collects from one vCenter as before. Press **More vCenters / VCF...** to add more
  vCenters (blank user/password fall back to the main window's) or to enter an **SDDC Manager** host, which
  discovers every vCenter it manages. All vCenters are collected in parallel; one failing does not stop the
  others. Server names and users are remembered between runs (never passwords).
* **CLI:** `-s a.corp,b.corp` collects several; `-vcf sddc.corp -u admin@vsphere.local` discovers the vCenters
  of a VCF instance (`-vcuser/-vcpass` if the vCenters use different credentials).

VCF discovery uses the SDDC Manager API (`POST /v1/tokens`, `GET /v1/vcenters`, falling back to `/v1/domains`)
and then logs in to each vCenter with the given credentials. It is covered by tests against a mock SDDC Manager,
not yet against a real VCF.

## Tabs

vCenters, Info, CPU, Memory, Disk, Partition, Network, Snapshot, Tools, CD, Host, Cluster, RP, Datastore,
Switch, Port, SC_VMK, NIC, HBA, dvSwitch, dvPort, License, Health.

Host includes `Cores per CPU` and `Licensable cores` = sockets x max(16, cores per socket).

Not (yet) replicated from RVTools: USB, FileInfo, MultiPath, performance history.

## Why it is fast

* One bulk property-collector retrieval per object type, all issued concurrently.
* VMs are fetched in parallel 200-VM shards.
* Only needed properties are requested; the xlsx is written with a streaming writer.

## Build

```bash
make            # linux/windows/darwin binaries in dist/
```
