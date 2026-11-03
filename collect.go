package main

import (
	"context"
	"sync"

	"github.com/vmware/govmomi/license"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/sync/errgroup"
)

// Inventory holds everything retrieved from vCenter / ESXi.
type Inventory struct {
	About    types.AboutInfo
	Entities map[types.ManagedObjectReference]mo.ManagedEntity // folders, datacenters, clusters, compute resources
	VMs      []mo.VirtualMachine
	Hosts    []mo.HostSystem
	Clusters []mo.ClusterComputeResource
	DS       []mo.Datastore
	RPs      []mo.ResourcePool
	DVPGs    []mo.DistributedVirtualPortgroup
	DVSs     []mo.DistributedVirtualSwitch
	Licenses []types.LicenseManagerLicenseInfo

	// derived lookups
	rpName   map[types.ManagedObjectReference]string
	hostName map[types.ManagedObjectReference]string
	dsName   map[types.ManagedObjectReference]string
	dvpgName map[string]string // portgroup key -> name
	hostVMs  map[types.ManagedObjectReference]int
	Warnings []string
}

const (
	vmChunk       = 200 // VMs per RetrieveProperties call
	maxConcurrent = 12  // parallel API calls in flight
)

var vmProps = []string{
	"name", "parent", "resourcePool", "datastore", "network",
	"config", "guest", "snapshot", "runtime", "summary",
	"overallStatus", "configStatus", "guestHeartbeatStatus",
}

// Collect pulls the whole inventory with all independent retrievals in parallel.
func Collect(ctx context.Context, c *vim25.Client) (*Inventory, error) {
	inv := &Inventory{About: c.ServiceContent.About}
	root := c.ServiceContent.RootFolder

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrent)
	var mu sync.Mutex
	warn := func(s string) { mu.Lock(); inv.Warnings = append(inv.Warnings, s); mu.Unlock() }

	// retrieve runs one container-view retrieval of a given kind into dst.
	retrieve := func(kind string, props []string, dst any, fatal bool) func() error {
		return func() error {
			m := view.NewManager(c)
			v, err := m.CreateContainerView(gctx, root, []string{kind}, true)
			if err != nil {
				return err
			}
			defer v.Destroy(context.Background())
			if err := v.Retrieve(gctx, []string{kind}, props, dst); err != nil {
				if fatal {
					return err
				}
				warn(kind + ": " + err.Error())
			}
			return nil
		}
	}

	g.Go(retrieve("HostSystem", []string{
		"name", "parent", "summary", "runtime", "config.network", "config.product",
		"config.storageDevice.hostBusAdapter", "config.network.dnsConfig", "overallStatus",
	}, &inv.Hosts, false))
	g.Go(retrieve("ClusterComputeResource", []string{"name", "parent", "summary", "configurationEx", "overallStatus", "configStatus"}, &inv.Clusters, false))
	g.Go(retrieve("Datastore", []string{"name", "parent", "summary", "host", "vm", "overallStatus"}, &inv.DS, false))
	g.Go(retrieve("ResourcePool", []string{"name", "parent", "config", "vm", "owner", "summary.quickStats"}, &inv.RPs, false))
	g.Go(retrieve("DistributedVirtualPortgroup", []string{"name", "key", "config", "parent"}, &inv.DVPGs, false))
	g.Go(retrieve("DistributedVirtualSwitch", []string{"name", "parent", "summary", "config"}, &inv.DVSs, false))

	g.Go(func() error {
		var ents []mo.ManagedEntity
		m := view.NewManager(c)
		v, err := m.CreateContainerView(gctx, root, []string{"Folder", "Datacenter", "ComputeResource"}, true)
		if err != nil {
			return err
		}
		defer v.Destroy(context.Background())
		if err := v.Retrieve(gctx, []string{"Folder", "Datacenter", "ComputeResource"}, []string{"name", "parent"}, &ents); err != nil {
			return err
		}
		inv.Entities = make(map[types.ManagedObjectReference]mo.ManagedEntity, len(ents))
		for _, e := range ents {
			inv.Entities[e.Self] = e
		}
		return nil
	})

	g.Go(func() error {
		lm := license.NewManager(c)
		infos, err := lm.List(gctx)
		if err != nil {
			warn("licenses: " + err.Error())
			return nil
		}
		inv.Licenses = infos
		return nil
	})

	// VMs: list refs cheaply, then fetch heavy properties in parallel shards.
	g.Go(func() error {
		var stub []mo.VirtualMachine
		m := view.NewManager(c)
		v, err := m.CreateContainerView(gctx, root, []string{"VirtualMachine"}, true)
		if err != nil {
			return err
		}
		defer v.Destroy(context.Background())
		if err := v.Retrieve(gctx, []string{"VirtualMachine"}, []string{"name"}, &stub); err != nil {
			return err
		}
		refs := make([]types.ManagedObjectReference, len(stub))
		for i := range stub {
			refs[i] = stub[i].Self
		}
		shards := make([][]mo.VirtualMachine, (len(refs)+vmChunk-1)/vmChunk)
		sg, sctx := errgroup.WithContext(gctx)
		sg.SetLimit(maxConcurrent)
		pc := property.DefaultCollector(c)
		for i := range shards {
			i := i
			lo, hi := i*vmChunk, min((i+1)*vmChunk, len(refs))
			sg.Go(func() error {
				return pc.Retrieve(sctx, refs[lo:hi], vmProps, &shards[i])
			})
		}
		if err := sg.Wait(); err != nil {
			return err
		}
		for _, s := range shards {
			inv.VMs = append(inv.VMs, s...)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	inv.index()
	return inv, nil
}

func (inv *Inventory) index() {
	inv.rpName = map[types.ManagedObjectReference]string{}
	inv.hostName = map[types.ManagedObjectReference]string{}
	inv.dsName = map[types.ManagedObjectReference]string{}
	inv.dvpgName = map[string]string{}
	inv.hostVMs = map[types.ManagedObjectReference]int{}
	for _, r := range inv.RPs {
		inv.rpName[r.Self] = r.Name
	}
	for _, h := range inv.Hosts {
		inv.hostName[h.Self] = h.Name
	}
	for _, d := range inv.DS {
		inv.dsName[d.Self] = d.Name
	}
	for _, p := range inv.DVPGs {
		inv.dvpgName[p.Key] = p.Name
	}
	for i := range inv.VMs {
		if h := inv.VMs[i].Runtime.Host; h != nil {
			inv.hostVMs[*h]++
		}
	}
	// hosts and clusters live in the entity map too (for ancestry walks)
	for _, h := range inv.Hosts {
		inv.Entities[h.Self] = mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: h.Self}, Name: h.Name, Parent: h.Parent}
	}
	for _, rp := range inv.RPs {
		inv.Entities[rp.Self] = mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: rp.Self}, Name: rp.Name, Parent: rp.Parent}
	}
}

// ancestor walks up the parent chain and returns the first ancestor of the given type.
func (inv *Inventory) ancestor(ref *types.ManagedObjectReference, typ string) string {
	for i := 0; ref != nil && i < 32; i++ {
		e, ok := inv.Entities[*ref]
		if !ok {
			return ""
		}
		if e.Self.Type == typ {
			return e.Name
		}
		ref = e.Parent
	}
	return ""
}

func (inv *Inventory) datacenter(ref *types.ManagedObjectReference) string {
	return inv.ancestor(ref, "Datacenter")
}

func (inv *Inventory) cluster(ref *types.ManagedObjectReference) string {
	return inv.ancestor(ref, "ClusterComputeResource")
}

// path returns the inventory path (/DC/vm/Folder) of an object's parent chain.
func (inv *Inventory) path(parent *types.ManagedObjectReference) string {
	var parts []string
	ref := parent
	for i := 0; ref != nil && i < 32; i++ {
		e, ok := inv.Entities[*ref]
		if !ok {
			break
		}
		parts = append(parts, e.Name)
		ref = e.Parent
	}
	s := ""
	for i := len(parts) - 1; i >= 0; i-- {
		s += "/" + parts[i]
	}
	return s
}

// hostCluster returns the datacenter and cluster of a host given its ref.
func (inv *Inventory) hostLoc(h *types.ManagedObjectReference) (host, dc, cluster string) {
	if h == nil {
		return "", "", ""
	}
	e := inv.Entities[*h]
	return inv.hostName[*h], inv.datacenter(e.Parent), inv.cluster(e.Parent)
}
