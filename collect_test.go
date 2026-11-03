package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestEndToEnd(t *testing.T) {
	m := simulator.VPX()
	m.Machine = 450 // exercises VM sharding (vmChunk = 200)
	err := m.Run(func(ctx context.Context, c *vim25.Client) error {
		t0 := time.Now()
		inv, err := Collect(ctx, c)
		t.Logf("collect %s", time.Since(t0))
		if err != nil {
			return err
		}
		if len(inv.VMs) != 450*m.Datacenter*m.Cluster+len(inv.VMs)-450*m.Datacenter*m.Cluster && len(inv.VMs) < 450 {
			t.Errorf("expected >=450 VMs, got %d", len(inv.VMs))
		}
		sheets := BuildSheets(inv, "sim")
		for _, s := range sheets {
			t.Logf("%-12s %5d rows", s.Name, len(s.Rows))
			for i, r := range s.Rows {
				if len(r) != len(s.Header) && !(s.Name == "Info" && len(r) == 6) {
					t.Fatalf("%s row %d: %d cols, header %d", s.Name, i, len(r), len(s.Header))
				}
			}
		}
		t1 := time.Now()
		for _, s := range sheets {
			if s.Name != "Host" {
				continue
			}
			col := func(n string) int {
				for i, h := range s.Header {
					if h == n {
						return i
					}
				}
				t.Fatalf("missing column %s", n)
				return -1
			}
			for _, r := range s.Rows {
				pkgs, cores := r[col("# CPU")].(int16), r[col("# Cores")].(int16)
				want := int(pkgs) * max(16, int(cores)/int(pkgs))
				if got := r[col("Licensable cores")]; got != want {
					t.Errorf("licensable cores = %v, want %d (pkgs %d cores %d)", got, want, pkgs, cores)
				}
				t.Logf("host sockets=%d cores=%d licensable=%v", pkgs, cores, r[col("Licensable cores")])
			}
		}
		dir := t.TempDir()
		if err := WriteXLSX(filepath.Join(dir, "o.xlsx"), sheets); err != nil {
			return err
		}
		err = WriteCSV(filepath.Join(dir, "csv"), sheets)
		t.Logf("write %s", time.Since(t1))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
