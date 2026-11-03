package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

// collectNilable returns every pointer/interface field reachable from v (to a small depth),
// so a test can blank each one in turn, mimicking the gaps real vCenter data has.
func collectNilable(v reflect.Value, out *[]reflect.Value, depth int) {
	if depth > 7 {
		return
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return
		}
		if v.CanSet() {
			*out = append(*out, v)
		}
		collectNilable(v.Elem(), out, depth+1)
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if v.CanSet() {
			*out = append(*out, v)
		}
		collectNilable(v.Elem(), out, depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				collectNilable(v.Field(i), out, depth+1)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len() && i < 3; i++ {
			collectNilable(v.Index(i), out, depth+1)
		}
	}
}

func tryBuild(inv *Inventory) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	before := len(inv.Warnings)
	BuildSheets(inv, "x")
	if len(inv.Warnings) > before {
		err = fmt.Errorf("%s", inv.Warnings[before])
	}
	inv.Warnings = inv.Warnings[:before]
	return
}

func TestNilFields(t *testing.T) {
	m := simulator.VPX()
	err := m.Run(func(ctx context.Context, c *vim25.Client) error {
		inv, err := Collect(ctx, c)
		if err != nil {
			return err
		}
		targets := map[string]reflect.Value{
			"vm": reflect.ValueOf(&inv.VMs[0]).Elem(), "host": reflect.ValueOf(&inv.Hosts[0]).Elem(),
			"cluster": reflect.ValueOf(&inv.Clusters[0]).Elem(), "ds": reflect.ValueOf(&inv.DS[0]).Elem(),
			"rp": reflect.ValueOf(&inv.RPs[0]).Elem(), "dvpg": reflect.ValueOf(&inv.DVPGs[0]).Elem(),
			"dvs": reflect.ValueOf(&inv.DVSs[0]).Elem(),
		}
		seen := map[string]bool{}
		for name, tv := range targets {
			var fields []reflect.Value
			collectNilable(tv, &fields, 0)
			for _, f := range fields {
				old := reflect.New(f.Type()).Elem()
				old.Set(f)
				f.Set(reflect.Zero(f.Type()))
				if err := tryBuild(inv); err != nil && !seen[err.Error()] {
					seen[err.Error()] = true
					t.Errorf("%s: nil %s -> %v", name, f.Type(), err)
				}
				f.Set(old)
			}
			t.Logf("%s: tried %d fields", name, len(fields))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
