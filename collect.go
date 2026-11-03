package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	wmu      sync.Mutex
}

// tuning controls how hard a vCenter is hit. A busy or slow vCenter gets the gentle profile on retry.
type tuning struct {
	concurrency int // parallel API calls in flight
	vmChunk     int // VMs per RetrieveProperties call
}

var (
	defaultTuning = tuning{concurrency: 12, vmChunk: 200}
	gentleTuning  = tuning{concurrency: 3, vmChunk: 50}
)

const maxWarnings = 200

var vmProps = []string{
	"name", "parent", "resourcePool", "datastore", "network",
	"config", "guest", "snapshot", "runtime", "summary",
	"overallStatus", "configStatus", "guestHeartbeatStatus",
}

// addWarning records a non-fatal problem (safe for concurrent use, capped).
func (inv *Inventory) addWarning(s string) {
	inv.wmu.Lock()
	defer inv.wmu.Unlock()
	switch {
	case len(inv.Warnings) < maxWarnings:
		inv.Warnings = append(inv.Warnings, s)
	case len(inv.Warnings) == maxWarnings:
		inv.Warnings = append(inv.Warnings, "further warnings suppressed")
	}
}

// sanitize reports and strips the per-object faults (for example NoPermission on a few VMs,
// or a VM deleted mid-collection). govmomi would otherwise fail the WHOLE retrieval because of
// one such object; here that object is kept with whatever properties could be read.
func (inv *Inventory) sanitize(raw []types.ObjectContent, kind string) []types.ObjectContent {
	for i := range raw {
		if len(raw[i].MissingSet) == 0 {
			continue
		}
		name := raw[i].Obj.Value
		for _, p := range raw[i].PropSet {
			if p.Name == "name" {
				if n, ok := p.Val.(string); ok {
					name = n
				}
			}
		}
		var parts []string
		for _, m := range raw[i].MissingSet {
			why := m.Fault.LocalizedMessage
			if why == "" {
				why = strings.TrimPrefix(fmt.Sprintf("%T", m.Fault.Fault), "*types.")
			}
			parts = append(parts, m.Path+": "+why)
			if len(parts) == 3 {
				break
			}
		}
		inv.addWarning(fmt.Sprintf("%s %q: some properties could not be read (%s)", kind, name, strings.Join(parts, "; ")))
		raw[i].MissingSet = nil
	}
	return raw
}

func destroyView(v *view.ContainerView) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // never block on a hung vCenter
	defer cancel()
	v.Destroy(ctx)
}

// Collect pulls the whole inventory with default tuning.
func Collect(ctx context.Context, c *vim25.Client) (*Inventory, error) {
	return CollectTuned(ctx, c, defaultTuning)
}

// CollectTuned pulls the whole inventory with all independent retrievals in parallel.
func CollectTuned(ctx context.Context, c *vim25.Client, tn tuning) (*Inventory, error) {
	inv := &Inventory{About: c.ServiceContent.About}
	root := c.ServiceContent.RootFolder

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(tn.concurrency + 8)
	sem := make(chan struct{}, tn.concurrency) // bounds concurrent API calls
	acquire := func() func() {
		select {
		case sem <- struct{}{}:
			return func() { <-sem }
		case <-gctx.Done():
			return func() {}
		}
	}

	// retrieve runs one container-view retrieval of a given kind into dst (a pointer to a slice).
	retrieve := func(kind string, props []string, dst any, fatal bool) func() error {
		return func() error {
			defer acquire()()
			m := view.NewManager(c)
			v, err := m.CreateContainerView(gctx, root, []string{kind}, true)
			if err != nil {
				if fatal {
					return err
				}
				inv.addWarning(kind + ": " + err.Error())
				return nil
			}
			defer destroyView(v)
			var raw []types.ObjectContent
			if err := v.Retrieve(gctx, []string{kind}, props, &raw); err != nil {
				if fatal {
					return err
				}
				inv.addWarning(kind + ": " + err.Error())
				return nil
			}
			if err := mo.LoadObjectContent(inv.sanitize(raw, kind), dst); err != nil {
				if fatal {
					return err
				}
				inv.addWarning(kind + ": " + err.Error())
			}
			return nil
		}
	}

	goSafe(g, retrieve("HostSystem", []string{
		"name", "parent", "summary", "runtime", "config.network", "config.product",
		"config.storageDevice.hostBusAdapter", "config.network.dnsConfig", "overallStatus",
	}, &inv.Hosts, false))
	goSafe(g, retrieve("ClusterComputeResource", []string{"name", "parent", "summary", "configurationEx", "overallStatus", "configStatus"}, &inv.Clusters, false))
	goSafe(g, retrieve("Datastore", []string{"name", "parent", "summary", "host", "vm", "overallStatus"}, &inv.DS, false))
	goSafe(g, retrieve("ResourcePool", []string{"name", "parent", "config", "vm", "owner", "summary.quickStats"}, &inv.RPs, false))
	goSafe(g, retrieve("DistributedVirtualPortgroup", []string{"name", "key", "config", "parent"}, &inv.DVPGs, false))
	goSafe(g, retrieve("DistributedVirtualSwitch", []string{"name", "parent", "summary", "config"}, &inv.DVSs, false))

	// folders / datacenters / compute resources give names and the parent chain (paths, datacenter, cluster)
	var ents []mo.ManagedEntity
	goSafe(g, func() error {
		defer acquire()()
		kinds := []string{"Folder", "Datacenter", "ComputeResource"}
		v, err := view.NewManager(c).CreateContainerView(gctx, root, kinds, true)
		if err != nil {
			inv.addWarning("inventory folders: " + err.Error())
			return nil
		}
		defer destroyView(v)
		var raw []types.ObjectContent
		if err := v.Retrieve(gctx, kinds, []string{"name", "parent"}, &raw); err != nil {
			inv.addWarning("inventory folders: " + err.Error())
			return nil
		}
		if err := mo.LoadObjectContent(inv.sanitize(raw, "folder"), &ents); err != nil {
			inv.addWarning("inventory folders: " + err.Error())
		}
		return nil
	})

	goSafe(g, func() error {
		defer acquire()()
		infos, err := license.NewManager(c).List(gctx)
		if err != nil {
			inv.addWarning("licenses: " + err.Error())
			return nil
		}
		inv.Licenses = infos
		return nil
	})

	// VMs: list refs cheaply, then fetch the heavy properties in parallel shards.
	goSafe(g, func() error {
		var raw []types.ObjectContent
		release := acquire()
		v, err := view.NewManager(c).CreateContainerView(gctx, root, []string{"VirtualMachine"}, true)
		if err != nil {
			release()
			return err
		}
		err = v.Retrieve(gctx, []string{"VirtualMachine"}, []string{"name"}, &raw)
		destroyView(v)
		release()
		if err != nil {
			return err
		}
		refs := make([]types.ManagedObjectReference, len(raw))
		for i := range raw {
			refs[i] = raw[i].Obj
		}
		shards := make([][]mo.VirtualMachine, (len(refs)+tn.vmChunk-1)/tn.vmChunk)
		sg, sctx := errgroup.WithContext(gctx)
		pc := property.DefaultCollector(c)
		for i := range shards {
			i := i
			lo, hi := i*tn.vmChunk, min((i+1)*tn.vmChunk, len(refs))
			goSafe(sg, func() error {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-sctx.Done():
					return sctx.Err()
				}
				var rc []types.ObjectContent
				if err := pc.Retrieve(sctx, refs[lo:hi], vmProps, &rc); err != nil {
					return err
				}
				return mo.LoadObjectContent(inv.sanitize(rc, "VM"), &shards[i])
			})
		}
		if err := sg.Wait(); err != nil {
			return err
		}
		for _, sh := range shards {
			inv.VMs = append(inv.VMs, sh...)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	inv.Entities = make(map[types.ManagedObjectReference]mo.ManagedEntity, len(ents))
	for _, e := range ents {
		inv.Entities[e.Self] = e
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

// goSafe runs f in the group, turning a panic in that goroutine into an error
// (an unrecovered panic in any goroutine would otherwise kill the whole process).
func goSafe(g *errgroup.Group, f func() error) {
	g.Go(func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				writeCrashLog(r)
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		return f()
	})
}
