package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Sheet is one tab of output (RVTools-style).
type Sheet struct {
	Name   string
	Header []string
	Rows   [][]any
}

const mib = 1024 * 1024

func bp(b *bool) string {
	if b == nil {
		return ""
	}
	if *b {
		return "True"
	}
	return "False"
}

func bs(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func ts(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func tv(t time.Time) string { return ts(&t) }

func ip64(p *int64) any {
	if p == nil {
		return ""
	}
	return *p
}

func shares(s *types.SharesInfo) any {
	if s == nil {
		return ""
	}
	return s.Shares
}

func dsFromPath(p string) string {
	if strings.HasPrefix(p, "[") {
		if i := strings.Index(p, "]"); i > 0 {
			return p[1:i]
		}
	}
	return ""
}

// BuildSheets converts the inventory into all output sheets. Per-VM sheets are
// produced in a single pass over the VM list.
func BuildSheets(inv *Inventory, server string) []*Sheet {
	vInfo := &Sheet{Name: "vInfo", Header: []string{"VM", "Powerstate", "Template", "Config status", "DNS Name", "Connection state", "Guest state", "Heartbeat", "Consolidation needed", "PowerOn", "Boot time", "CPUs", "Memory MiB", "NICs", "Disks", "Provisioned MiB", "In Use MiB", "Unshared MiB", "Path", "Annotation", "Datacenter", "Cluster", "Host", "Folder", "Resource pool", "OS according to the configuration file", "OS according to the VMware Tools", "VM UUID", "Instance UUID", "VM ID", "HW version", "Firmware", "CBT", "Primary IP Address", "Network #1", "Network #2", "Network #3", "Network #4"}}
	vCPU := &Sheet{Name: "vCPU", Header: []string{"VM", "Powerstate", "Template", "CPUs", "Sockets", "Cores p/s", "Max MHz", "Overall MHz", "Reservation MHz", "Limit MHz", "Shares", "Hot Add", "Hot Remove", "Datacenter", "Cluster", "Host"}}
	vMem := &Sheet{Name: "vMemory", Header: []string{"VM", "Powerstate", "Template", "Size MiB", "Consumed MiB", "Active MiB", "Ballooned MiB", "Swapped MiB", "Compressed KiB", "Reservation MiB", "Limit MiB", "Shares", "Hot Add", "Datacenter", "Cluster", "Host"}}
	vDisk := &Sheet{Name: "vDisk", Header: []string{"VM", "Powerstate", "Template", "Disk", "Disk Key", "Capacity MiB", "Thin", "Eagerly Scrub", "Mode", "Raw", "Disk Path", "Datastore", "Controller", "Unit #", "Datacenter", "Cluster", "Host"}}
	vPart := &Sheet{Name: "vPartition", Header: []string{"VM", "Powerstate", "Disk", "Capacity MiB", "Consumed MiB", "Free MiB", "Free %", "Datacenter", "Cluster", "Host"}}
	vNet := &Sheet{Name: "vNetwork", Header: []string{"VM", "Powerstate", "NIC label", "Adapter", "Network", "Connected", "Starts Connected", "Mac Address", "Mac Type", "IPv4 Address", "IPv6 Address", "Datacenter", "Cluster", "Host"}}
	vSnap := &Sheet{Name: "vSnapshot", Header: []string{"VM", "Powerstate", "Snapshot", "Description", "Date / time", "Quiesced", "State", "Path", "Datacenter", "Cluster", "Host"}}
	vTools := &Sheet{Name: "vTools", Header: []string{"VM", "Powerstate", "Tools", "Tools version", "Version status", "Upgradeable", "Required Version", "Datacenter", "Cluster", "Host"}}
	vCD := &Sheet{Name: "vCD", Header: []string{"VM", "Powerstate", "Device Node", "Connected", "Starts Connected", "Device Type", "Backing", "Datacenter", "Cluster", "Host"}}
	vHealth := &Sheet{Name: "vHealth", Header: []string{"Name", "Type", "Overall status", "Config status", "Message"}}

	for i := range inv.VMs {
		vm := &inv.VMs[i]
		cfg := vm.Config
		hostN, dc, cl := inv.hostLoc(vm.Runtime.Host)
		power := string(vm.Runtime.PowerState)
		tmpl := ""
		if cfg != nil {
			tmpl = bs(cfg.Template)
		}
		if cfg == nil {
			vInfo.Rows = append(vInfo.Rows, []any{vm.Name, power, "", string(vm.ConfigStatus), "", string(vm.Runtime.ConnectionState)})
			continue
		}

		// ---- device pass
		devs := object.VirtualDeviceList(cfg.Hardware.Device)
		ctrlName := map[int32]string{}
		for _, d := range devs {
			if _, ok := d.(types.BaseVirtualController); ok {
				ctrlName[d.GetVirtualDevice().Key] = devs.Name(d)
			}
		}
		nics, disks := 0, 0
		var nets []string
		for _, d := range devs {
			switch dev := d.(type) {
			case *types.VirtualDisk:
				disks++
				var path, mode, ds string
				var thin, scrub *bool
				raw := "False"
				switch b := dev.Backing.(type) {
				case *types.VirtualDiskFlatVer2BackingInfo:
					path, mode, thin, scrub = b.FileName, b.DiskMode, b.ThinProvisioned, b.EagerlyScrub
				case *types.VirtualDiskSparseVer2BackingInfo:
					path, mode = b.FileName, b.DiskMode
				case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
					path, mode, raw = b.FileName, b.DiskMode, "True"
				case *types.VirtualDiskSeSparseBackingInfo:
					path, mode = b.FileName, b.DiskMode
				}
				ds = dsFromPath(path)
				unit := ""
				if dev.UnitNumber != nil {
					unit = fmt.Sprint(*dev.UnitNumber)
				}
				vDisk.Rows = append(vDisk.Rows, []any{vm.Name, power, tmpl, dev.DeviceInfo.GetDescription().Label, dev.Key, dev.CapacityInBytes / mib, bp(thin), bp(scrub), mode, raw, path, ds, ctrlName[dev.ControllerKey], unit, dc, cl, hostN})
			case *types.VirtualCdrom:
				var backing string
				switch b := dev.Backing.(type) {
				case *types.VirtualCdromIsoBackingInfo:
					backing = b.FileName
				case *types.VirtualCdromAtapiBackingInfo:
					backing = b.DeviceName
				case *types.VirtualCdromPassthroughBackingInfo:
					backing = b.DeviceName
				}
				var conn, start string
				if dev.Connectable != nil {
					conn, start = bs(dev.Connectable.Connected), bs(dev.Connectable.StartConnected)
				}
				vCD.Rows = append(vCD.Rows, []any{vm.Name, power, dev.DeviceInfo.GetDescription().Label, conn, start, devs.Type(d), backing, dc, cl, hostN})
			default:
				if card, ok := d.(types.BaseVirtualEthernetCard); ok {
					nics++
					c := card.GetVirtualEthernetCard()
					network := nicNetwork(inv, c)
					nets = append(nets, network)
					var v4, v6 []string
					if vm.Guest != nil {
						for _, gn := range vm.Guest.Net {
							if gn.DeviceConfigId != c.Key {
								continue
							}
							for _, a := range gn.IpAddress {
								if p := net.ParseIP(a); p != nil && p.To4() != nil {
									v4 = append(v4, a)
								} else if p != nil {
									v6 = append(v6, a)
								}
							}
						}
					}
					var conn, start string
					if c.Connectable != nil {
						conn, start = bs(c.Connectable.Connected), bs(c.Connectable.StartConnected)
					}
					vNet.Rows = append(vNet.Rows, []any{vm.Name, power, c.DeviceInfo.GetDescription().Label, devs.Type(d), network, conn, start, c.MacAddress, c.AddressType, strings.Join(v4, ", "), strings.Join(v6, ", "), dc, cl, hostN})
				}
			}
		}

		// ---- summary / vInfo
		var committed, uncommitted, unshared int64
		if s := vm.Summary.Storage; s != nil {
			committed, uncommitted, unshared = s.Committed, s.Uncommitted, s.Unshared
		}
		var dns, guestState, toolsOS, primaryIP, toolsRun, toolsVer, toolsVerStatus, toolsStatus string
		if g := vm.Guest; g != nil {
			dns, guestState, toolsOS, primaryIP = g.HostName, g.GuestState, g.GuestFullName, g.IpAddress
			toolsRun, toolsVer, toolsVerStatus = g.ToolsRunningStatus, g.ToolsVersion, g.ToolsVersionStatus2
			toolsStatus = string(g.ToolsStatus)
		}
		_ = toolsStatus
		for len(nets) < 4 {
			nets = append(nets, "")
		}
		vmPath := cfg.Files.VmPathName
		vInfo.Rows = append(vInfo.Rows, []any{
			vm.Name, power, tmpl, string(vm.ConfigStatus), dns, string(vm.Runtime.ConnectionState), guestState,
			string(vm.GuestHeartbeatStatus), bs(vm.Runtime.ConsolidationNeeded), ts(vm.Runtime.BootTime), ts(vm.Runtime.BootTime),
			cfg.Hardware.NumCPU, cfg.Hardware.MemoryMB, nics, disks,
			(committed + uncommitted) / mib, committed / mib, unshared / mib,
			vmPath, cfg.Annotation, dc, cl, hostN, strings.TrimSpace(inv.path(vm.Parent)),
			inv.rpName[derefRef(vm.ResourcePool)], cfg.GuestFullName, toolsOS,
			cfg.Uuid, cfg.InstanceUuid, vm.Self.Value, cfg.Version, cfg.Firmware, bp(cfg.ChangeTrackingEnabled),
			primaryIP, nets[0], nets[1], nets[2], nets[3],
		})
		// "PowerOn" is the last power-on == boot time on vSphere; kept for RVTools parity.

		// ---- vCPU / vMemory
		cps := int32(1)
		if p := cfg.Hardware.NumCoresPerSocket; p != nil && *p > 0 {
			cps = *p
		}
		qs := vm.Summary.QuickStats
		vCPU.Rows = append(vCPU.Rows, []any{vm.Name, power, tmpl, cfg.Hardware.NumCPU, cfg.Hardware.NumCPU / cps, cps,
			vm.Runtime.MaxCpuUsage, qs.OverallCpuUsage, ip64(cfg.CpuAllocation.Reservation), ip64(cfg.CpuAllocation.Limit),
			shares(cfg.CpuAllocation.Shares), bp(cfg.CpuHotAddEnabled), bp(cfg.CpuHotRemoveEnabled), dc, cl, hostN})
		vMem.Rows = append(vMem.Rows, []any{vm.Name, power, tmpl, cfg.Hardware.MemoryMB, qs.HostMemoryUsage, qs.GuestMemoryUsage,
			qs.BalloonedMemory, qs.SwappedMemory, qs.CompressedMemory, ip64(cfg.MemoryAllocation.Reservation), ip64(cfg.MemoryAllocation.Limit),
			shares(cfg.MemoryAllocation.Shares), bp(cfg.MemoryHotAddEnabled), dc, cl, hostN})

		// ---- partitions
		if vm.Guest != nil {
			for _, d := range vm.Guest.Disk {
				free := 0.0
				if d.Capacity > 0 {
					free = float64(d.FreeSpace) * 100 / float64(d.Capacity)
				}
				vPart.Rows = append(vPart.Rows, []any{vm.Name, power, d.DiskPath, d.Capacity / mib, (d.Capacity - d.FreeSpace) / mib, d.FreeSpace / mib, fmt.Sprintf("%.1f", free), dc, cl, hostN})
			}
		}

		// ---- tools
		upg := ""
		if toolsVerStatus != "" {
			upg = bs(toolsVerStatus == "guestToolsNeedUpgrade")
		}
		vTools.Rows = append(vTools.Rows, []any{vm.Name, power, toolsRun, toolsVer, toolsVerStatus, upg, cfg.Tools.ToolsVersion, dc, cl, hostN})

		// ---- snapshots
		if vm.Snapshot != nil {
			var walk func(list []types.VirtualMachineSnapshotTree, path string)
			walk = func(list []types.VirtualMachineSnapshotTree, path string) {
				for _, s := range list {
					p := path + "/" + s.Name
					vSnap.Rows = append(vSnap.Rows, []any{vm.Name, power, s.Name, s.Description, tv(s.CreateTime), bs(s.Quiesced), string(s.State), p, dc, cl, hostN})
					walk(s.ChildSnapshotList, p)
				}
			}
			walk(vm.Snapshot.RootSnapshotList, "")
		}

		// ---- health
		if vm.OverallStatus != types.ManagedEntityStatusGreen && vm.OverallStatus != types.ManagedEntityStatusGray {
			vHealth.Rows = append(vHealth.Rows, []any{vm.Name, "VirtualMachine", string(vm.OverallStatus), string(vm.ConfigStatus), ""})
		}
	}

	sheets := []*Sheet{vInfo, vCPU, vMem, vDisk, vPart, vNet, vSnap, vTools, vCD,
		hostSheet(inv), clusterSheet(inv), rpSheet(inv), dsSheet(inv)}
	sheets = append(sheets, hostNetSheets(inv)...)
	sheets = append(sheets, dvSheets(inv)...)
	sheets = append(sheets, licenseSheet(inv), sourceSheet(inv, server))
	for _, h := range inv.Hosts {
		if h.OverallStatus != types.ManagedEntityStatusGreen && h.OverallStatus != types.ManagedEntityStatusGray {
			vHealth.Rows = append(vHealth.Rows, []any{h.Name, "HostSystem", string(h.OverallStatus), "", ""})
		}
	}
	for _, d := range inv.DS {
		if d.OverallStatus != types.ManagedEntityStatusGreen && d.OverallStatus != types.ManagedEntityStatusGray {
			vHealth.Rows = append(vHealth.Rows, []any{d.Name, "Datastore", string(d.OverallStatus), "", ""})
		}
	}
	sheets = append(sheets, vHealth)
	for _, sh := range sheets {
		sh.Name = tabName(sh.Name)
	}
	return sheets
}

// tabName drops the RVTools-style leading lowercase 'v' (vInfo -> Info, vSC_VMK -> SC_VMK).
// Names such as dvSwitch are left alone.
func tabName(n string) string {
	if len(n) > 1 && n[0] == 'v' && n[1] >= 'A' && n[1] <= 'Z' {
		return n[1:]
	}
	return n
}

func derefRef(r *types.ManagedObjectReference) types.ManagedObjectReference {
	if r == nil {
		return types.ManagedObjectReference{}
	}
	return *r
}

func nicNetwork(inv *Inventory, c *types.VirtualEthernetCard) string {
	switch b := c.Backing.(type) {
	case *types.VirtualEthernetCardNetworkBackingInfo:
		return b.DeviceName
	case *types.VirtualEthernetCardDistributedVirtualPortBackingInfo:
		if n, ok := inv.dvpgName[b.Port.PortgroupKey]; ok {
			return n
		}
		return b.Port.PortgroupKey
	case *types.VirtualEthernetCardOpaqueNetworkBackingInfo:
		return b.OpaqueNetworkId
	}
	return ""
}

func hostSheet(inv *Inventory) *Sheet {
	s := &Sheet{Name: "vHost", Header: []string{"Host", "Datacenter", "Cluster", "Connection state", "In Maintenance Mode", "Power state", "CPU Model", "Speed MHz", "# CPU", "# Cores", "# Threads", "Cores per CPU", "Licensable cores", "CPU usage MHz", "CPU usage %", "# Memory MiB", "Memory usage MiB", "Memory usage %", "# VMs", "Vendor", "Model", "Serial / Service tag", "UUID", "ESX Version", "Build", "Boot time", "# NICs", "# HBAs", "Overall status"}}
	for _, h := range inv.Hosts {
		hw := h.Summary.Hardware
		e := inv.Entities[h.Self]
		_ = e
		dc, cl := inv.datacenter(h.Parent), inv.cluster(h.Parent)
		var model, cpu, vendor, serial, uuid string
		var mhz int32
		var pkgs, cores, threads int16
		var memMiB int64
		var nn, nh int32
		if hw != nil {
			model, cpu, vendor, uuid = hw.Model, hw.CpuModel, hw.Vendor, hw.Uuid
			mhz, pkgs, cores, threads = hw.CpuMhz, hw.NumCpuPkgs, hw.NumCpuCores, hw.NumCpuThreads
			memMiB = hw.MemorySize / mib
			nn, nh = hw.NumNics, hw.NumHBAs
			for _, o := range hw.OtherIdentifyingInfo {
				if o.IdentifierType != nil && (o.IdentifierType.GetElementDescription().Key == "ServiceTag" || o.IdentifierType.GetElementDescription().Key == "SerialNumberTag") {
					serial = o.IdentifierValue
				}
			}
		}
		var ver, build string
		if p := h.Summary.Config.Product; p != nil {
			ver, build = p.FullName, p.Build
		}
		// Licensable cores: each CPU socket is licensed for at least 16 cores.
		var coresPer, licensable any = "", ""
		if pkgs > 0 {
			per := int(cores) / int(pkgs)
			coresPer, licensable = per, int(pkgs)*max(16, per)
		}
		cpuPct, memPct := "", ""
		if cap := int64(mhz) * int64(cores); cap > 0 {
			cpuPct = fmt.Sprintf("%.1f", float64(h.Summary.QuickStats.OverallCpuUsage)*100/float64(cap))
		}
		if memMiB > 0 {
			memPct = fmt.Sprintf("%.1f", float64(h.Summary.QuickStats.OverallMemoryUsage)*100/float64(memMiB))
		}
		s.Rows = append(s.Rows, []any{h.Name, dc, cl, string(h.Runtime.ConnectionState), bs(h.Runtime.InMaintenanceMode), string(h.Runtime.PowerState),
			cpu, mhz, pkgs, cores, threads, coresPer, licensable, h.Summary.QuickStats.OverallCpuUsage, cpuPct, memMiB, h.Summary.QuickStats.OverallMemoryUsage, memPct,
			inv.hostVMs[h.Self], vendor, model, serial, uuid, ver, build, ts(h.Runtime.BootTime), nn, nh, string(h.OverallStatus)})
	}
	sort.SliceStable(s.Rows, func(i, j int) bool { return fmt.Sprint(s.Rows[i][0]) < fmt.Sprint(s.Rows[j][0]) })
	return s
}

func clusterSheet(inv *Inventory) *Sheet {
	s := &Sheet{Name: "vCluster", Header: []string{"Name", "Datacenter", "Config status", "Overall status", "# Hosts", "# Effective hosts", "Total CPU MHz", "Effective CPU MHz", "Total Memory MiB", "Effective Memory MiB", "# Cores", "# CPU threads", "HA enabled", "Admission control", "Failover level", "DRS enabled", "DRS behavior", "vMotion rate"}}
	for _, c := range inv.Clusters {
		sm := c.Summary.GetComputeResourceSummary()
		var ha, adm, fl, drs, beh, rate string
		var haLevel int32
		if cx, ok := c.ConfigurationEx.(*types.ClusterConfigInfoEx); ok {
			if cx.DasConfig.Enabled != nil {
				ha = bp(cx.DasConfig.Enabled)
			}
			adm = bp(cx.DasConfig.AdmissionControlEnabled)
			if cx.DasConfig.AdmissionControlPolicy != nil {
				if p, ok := cx.DasConfig.AdmissionControlPolicy.(*types.ClusterFailoverLevelAdmissionControlPolicy); ok {
					haLevel = p.FailoverLevel
					fl = fmt.Sprint(haLevel)
				}
			}
			drs = bp(cx.DrsConfig.Enabled)
			beh = string(cx.DrsConfig.DefaultVmBehavior)
			rate = fmt.Sprint(cx.DrsConfig.VmotionRate)
		}
		s.Rows = append(s.Rows, []any{c.Name, inv.datacenter(c.Parent), string(c.ConfigStatus), string(c.OverallStatus), sm.NumHosts, sm.NumEffectiveHosts,
			sm.TotalCpu, sm.EffectiveCpu, sm.TotalMemory / mib, sm.EffectiveMemory, sm.NumCpuCores, sm.NumCpuThreads, ha, adm, fl, drs, beh, rate})
	}
	return s
}

func rpSheet(inv *Inventory) *Sheet {
	s := &Sheet{Name: "vRP", Header: []string{"Resource Pool", "Datacenter", "Cluster", "CPU reservation MHz", "CPU limit MHz", "CPU shares", "Mem reservation MiB", "Mem limit MiB", "Mem shares", "# VMs"}}
	for _, r := range inv.RPs {
		s.Rows = append(s.Rows, []any{r.Name, inv.datacenter(r.Parent), inv.cluster(r.Parent), ip64(r.Config.CpuAllocation.Reservation), ip64(r.Config.CpuAllocation.Limit), shares(r.Config.CpuAllocation.Shares),
			ip64(r.Config.MemoryAllocation.Reservation), ip64(r.Config.MemoryAllocation.Limit), shares(r.Config.MemoryAllocation.Shares), len(r.Vm)})
	}
	return s
}

func dsSheet(inv *Inventory) *Sheet {
	s := &Sheet{Name: "vDatastore", Header: []string{"Name", "Datacenter", "Type", "Capacity MiB", "Free MiB", "Provisioned MiB", "In Use MiB", "Free %", "# VMs", "# Hosts", "URL", "Accessible", "Maintenance mode", "Overall status"}}
	for _, d := range inv.DS {
		sm := d.Summary
		free := 0.0
		if sm.Capacity > 0 {
			free = float64(sm.FreeSpace) * 100 / float64(sm.Capacity)
		}
		prov := sm.Capacity - sm.FreeSpace + derefInt(sm.Uncommitted)
		s.Rows = append(s.Rows, []any{d.Name, inv.datacenter(d.Parent), sm.Type, sm.Capacity / mib, sm.FreeSpace / mib, prov / mib, (sm.Capacity - sm.FreeSpace) / mib,
			fmt.Sprintf("%.1f", free), len(d.Vm), len(d.Host), sm.Url, bs(sm.Accessible), sm.MaintenanceMode, string(d.OverallStatus)})
	}
	return s
}

func derefInt(p int64) int64 { return p }

func hostNetSheets(inv *Inventory) []*Sheet {
	vsw := &Sheet{Name: "vSwitch", Header: []string{"Host", "Datacenter", "Cluster", "Switch", "# Ports", "Free Ports", "MTU", "Uplinks"}}
	vpt := &Sheet{Name: "vPort", Header: []string{"Host", "Datacenter", "Cluster", "Port Group", "Switch", "VLAN"}}
	vmk := &Sheet{Name: "vSC_VMK", Header: []string{"Host", "Datacenter", "Cluster", "Device", "Port Group", "IP Address", "Subnet mask", "MAC", "MTU", "DHCP"}}
	vnic := &Sheet{Name: "vNIC", Header: []string{"Host", "Datacenter", "Cluster", "Network Device", "Driver", "Speed Mb", "Duplex", "MAC"}}
	vhba := &Sheet{Name: "vHBA", Header: []string{"Host", "Datacenter", "Cluster", "Device", "Type", "Status", "Model", "Driver", "WWN / IQN"}}
	for _, h := range inv.Hosts {
		dc, cl := inv.datacenter(h.Parent), inv.cluster(h.Parent)
		if n := h.Config; n != nil && n.Network != nil {
			for _, sw := range n.Network.Vswitch {
				var up []string
				for _, p := range sw.Pnic {
					up = append(up, p)
				}
				vsw.Rows = append(vsw.Rows, []any{h.Name, dc, cl, sw.Name, sw.NumPorts, sw.NumPortsAvailable, sw.Mtu, strings.Join(up, ", ")})
			}
			for _, pg := range n.Network.Portgroup {
				vpt.Rows = append(vpt.Rows, []any{h.Name, dc, cl, pg.Spec.Name, pg.Spec.VswitchName, pg.Spec.VlanId})
			}
			for _, v := range n.Network.Vnic {
				var ipa, mask, mac string
				dhcp := ""
				if v.Spec.Ip != nil {
					ipa, mask, dhcp = v.Spec.Ip.IpAddress, v.Spec.Ip.SubnetMask, bs(v.Spec.Ip.Dhcp)
				}
				mac = v.Spec.Mac
				vmk.Rows = append(vmk.Rows, []any{h.Name, dc, cl, v.Device, v.Portgroup, ipa, mask, mac, v.Spec.Mtu, dhcp})
			}
			for _, p := range n.Network.Pnic {
				speed, duplex := "", ""
				if p.LinkSpeed != nil {
					speed, duplex = fmt.Sprint(p.LinkSpeed.SpeedMb), bs(p.LinkSpeed.Duplex)
				}
				vnic.Rows = append(vnic.Rows, []any{h.Name, dc, cl, p.Device, p.Driver, speed, duplex, p.Mac})
			}
		}
		if h.Config != nil && h.Config.StorageDevice != nil {
			for _, b := range h.Config.StorageDevice.HostBusAdapter {
				a := b.GetHostHostBusAdapter()
				id, typ := "", strings.TrimPrefix(fmt.Sprintf("%T", b), "*types.Host")
				switch t := b.(type) {
				case *types.HostFibreChannelHba:
					id = fmt.Sprintf("%016x", uint64(t.PortWorldWideName))
				case *types.HostInternetScsiHba:
					id = t.IScsiName
				}
				vhba.Rows = append(vhba.Rows, []any{h.Name, dc, cl, a.Device, typ, a.Status, a.Model, a.Driver, id})
			}
		}
	}
	return []*Sheet{vsw, vpt, vmk, vnic, vhba}
}

func dvSheets(inv *Inventory) []*Sheet {
	dvs := &Sheet{Name: "dvSwitch", Header: []string{"Switch", "Datacenter", "Version", "Vendor", "UUID", "# Ports", "# Hosts", "MTU", "Overall status"}}
	for _, d := range inv.DVSs {
		var ver, vendor string
		if p := d.Summary.ProductInfo; p != nil {
			ver, vendor = p.Version, p.Vendor
		}
		var mtu int32
		if d.Config != nil {
			if c, ok := d.Config.(*types.VMwareDVSConfigInfo); ok {
				mtu = c.MaxMtu
			}
		}
		dvs.Rows = append(dvs.Rows, []any{d.Name, inv.datacenter(d.Parent), ver, vendor, d.Uuid, d.Summary.NumPorts, len(d.Summary.HostMember), mtu, string(d.OverallStatus)})
	}
	dvp := &Sheet{Name: "dvPort", Header: []string{"Port Group", "Datacenter", "Switch UUID", "Type", "# Ports", "VLAN", "Key"}}
	for _, p := range inv.DVPGs {
		vlan := ""
		if p.Config.DefaultPortConfig != nil {
			if c, ok := p.Config.DefaultPortConfig.(*types.VMwareDVSPortSetting); ok && c.Vlan != nil {
				switch v := c.Vlan.(type) {
				case *types.VmwareDistributedVirtualSwitchVlanIdSpec:
					vlan = fmt.Sprint(v.VlanId)
				case *types.VmwareDistributedVirtualSwitchTrunkVlanSpec:
					var r []string
					for _, x := range v.VlanId {
						r = append(r, fmt.Sprintf("%d-%d", x.Start, x.End))
					}
					vlan = strings.Join(r, ",")
				case *types.VmwareDistributedVirtualSwitchPvlanSpec:
					vlan = fmt.Sprintf("pvlan %d", v.PvlanId)
				}
			}
		}
		dvp.Rows = append(dvp.Rows, []any{p.Name, inv.datacenter(p.Parent), "", p.Config.Type, p.Config.NumPorts, vlan, p.Key})
	}
	return []*Sheet{dvs, dvp}
}

func licenseSheet(inv *Inventory) *Sheet {
	s := &Sheet{Name: "vLicense", Header: []string{"Name", "Key", "Labels", "Cost Unit", "Total", "Used", "Expiration"}}
	for _, l := range inv.Licenses {
		exp := ""
		for _, p := range l.Properties {
			if p.Key == "expirationDate" {
				if t, ok := p.Value.(time.Time); ok {
					exp = tv(t)
				}
			}
		}
		var labels []string
		for _, lb := range l.Labels {
			labels = append(labels, lb.Key+"="+lb.Value)
		}
		s.Rows = append(s.Rows, []any{l.Name, l.LicenseKey, strings.Join(labels, ";"), l.CostUnit, l.Total, l.Used, exp})
	}
	return s
}

func sourceSheet(inv *Inventory, server string) *Sheet {
	a := inv.About
	return &Sheet{Name: "vSource", Header: []string{"Server", "Name", "Version", "Build", "OS Type", "API type", "API version", "Instance UUID", "Collected"},
		Rows: [][]any{{server, a.FullName, a.Version, a.Build, a.OsType, a.ApiType, a.ApiVersion, a.InstanceUuid, time.Now().Format("2006-01-02 15:04:05")}}}
}

var _ = mo.VirtualMachine{}
