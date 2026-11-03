package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Regression: a failing vCenter must be reported (the GUI once crashed when an error was
// swallowed and an empty result was shown) and must not stop the app.
func TestCollectManyReportsErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sheets, res := CollectMany(ctx, []Target{{Server: "127.0.0.1:1", User: "u", Password: "p", Insecure: true}}, Options{})
	if len(res) != 1 || res[0].OK || res[0].Error == "" {
		t.Fatalf("expected a failed result with an error, got %+v", res)
	}
	if len(sheets) == 0 || sheets[0].Name != "vCenters" || sheets[0].Rows[0][1] != "Failed" {
		t.Fatal("vCenters tab should list the failed connection")
	}
}

func simVC(t *testing.T) string {
	m := simulator.VPX()
	m.Machine = 3
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	m.Service.TLS = new(tls.Config)
	s := m.Service.NewServer()
	t.Cleanup(func() { s.Close(); m.Remove() })
	return s.URL.Host
}

// Two vCenters collected together: rows are merged and every row names its vCenter.
func TestCollectManyMergesAndTags(t *testing.T) {
	a, b := simVC(t), simVC(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sheets, res := CollectMany(ctx, []Target{
		{Server: a, User: "u", Password: "p", Source: "Manual", Insecure: true},
		{Server: b, User: "u", Password: "p", Source: "VCF: x / d1", Insecure: true},
		{Server: "127.0.0.1:1", User: "u", Password: "p", Insecure: true},
	}, Options{})
	if !res[0].OK || !res[1].OK || res[2].OK {
		t.Fatalf("results: %+v", res)
	}
	byName := map[string]*Sheet{}
	for _, s := range sheets {
		byName[s.Name] = s
		if s.Header[0] != "vCenter" {
			t.Errorf("%s: first column is %q", s.Name, s.Header[0])
		}
		for _, r := range s.Rows {
			if len(r) > 0 && len(r) != len(s.Header) && s.Name != "Info" {
				t.Errorf("%s: row has %d cols, header %d", s.Name, len(r), len(s.Header))
			}
		}
	}
	if sheets[0].Name != "vCenters" || len(sheets[0].Rows) != 3 {
		t.Fatalf("vCenters tab: %+v", sheets[0].Rows)
	}
	seen := map[any]int{}
	for _, r := range byName["Info"].Rows {
		seen[r[0]]++
	}
	lbA, lbB := vcLabel(a), vcLabel(b)
	if seen[lbA] == 0 || seen[lbB] == 0 || len(seen) != 2 {
		t.Fatalf("Info rows per vCenter: %v (want %s and %s)", seen, lbA, lbB)
	}
	t.Logf("rows per vCenter: %v; hosts: %d", seen, len(byName["Host"].Rows))
}

func TestDiscoverVCF(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/tokens" && r.Method == "POST":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["password"] != "good" {
				http.Error(w, `{"message":"bad credentials"}`, 401)
				return
			}
			w.Write([]byte(`{"accessToken":"tok"}`))
		case r.URL.Path == "/v1/vcenters" && r.Header.Get("Authorization") == "Bearer tok":
			w.Write([]byte(`{"elements":[{"fqdn":"vc-mgmt.corp","version":"8.0","domain":{"name":"mgmt"}},{"fqdn":"vc-w1.corp","domain":{"name":"wld1"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	vcs, err := DiscoverVCF(ctx, srv.URL, "admin", "good", true)
	if err != nil || len(vcs) != 2 || vcs[0].FQDN != "vc-mgmt.corp" || vcs[1].Domain != "wld1" {
		t.Fatalf("got %+v err %v", vcs, err)
	}
	if _, err := DiscoverVCF(ctx, srv.URL, "admin", "bad", true); err == nil {
		t.Fatal("expected login failure")
	}
}

func TestDiscoverVCFOps(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/suite-api/api/auth/token/acquire" && r.Method == "POST":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["password"] != "good" || b["authSource"] != "vIDB" {
				http.Error(w, `{"message":"bad credentials"}`, 401)
				return
			}
			w.Write([]byte(`{"token":"abc","validity":1}`))
		case r.URL.Path == "/suite-api/api/adapters" && r.URL.Query().Get("adapterKindKey") == "VMWARE" && r.Header.Get("Authorization") == "OpsToken abc":
			w.Write([]byte(`{"adapterInstancesInfoDto":[
			 {"resourceKey":{"name":"vc-mgmt","adapterKindKey":"VMWARE","resourceIdentifiers":[{"identifierType":{"name":"VCURL"},"value":"vc-mgmt.corp"},{"identifierType":{"name":"AUTO_CHANGES"},"value":"x"}]}},
			 {"resourceKey":{"name":"vc-w1","resourceIdentifiers":[{"identifierType":{"name":"VCURL"},"value":"https://vc-w1.corp/sdk"}]}},
			 {"resourceKey":{"name":"dup","resourceIdentifiers":[{"identifierType":{"name":"VCURL"},"value":"VC-MGMT.corp"}]}},
			 {"resourceKey":{"name":"no-url","resourceIdentifiers":[]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	vcs, err := DiscoverVCFOps(context.Background(), srv.URL, "admin", "good", "vIDB", true)
	if err != nil || len(vcs) != 2 || vcs[0].FQDN != "vc-mgmt.corp" || vcs[0].Domain != "vc-mgmt" || vcs[1].FQDN != "vc-w1.corp" {
		t.Fatalf("got %+v err %v", vcs, err)
	}
	if _, err := DiscoverVCFOps(context.Background(), srv.URL, "admin", "bad", "vIDB", true); err == nil {
		t.Fatal("expected login failure")
	}
}

// A vCenter that accepts the connection but never answers must not hang the collection:
// it is reported as timed out while the healthy vCenter still succeeds.
func TestCollectManyHungVCenter(t *testing.T) {
	good := simVC(t)
	hung := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never respond (but let go eventually so the test server can shut down)
		case <-r.Context().Done():
		case <-time.After(6 * time.Second):
		}
	}))
	defer hung.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t0 := time.Now()
	sheets, res := CollectMany(ctx, []Target{
		{Server: hung.Listener.Addr().String(), User: "u", Password: "p", Insecure: true},
		{Server: good, User: "u", Password: "p", Insecure: true},
	}, Options{Timeout: 3 * time.Second})
	if time.Since(t0) > 30*time.Second {
		t.Fatalf("took too long: %s", time.Since(t0))
	}
	if res[0].OK || !strings.Contains(res[0].Error, "Timed out") {
		t.Fatalf("hung vCenter result: %+v", res[0])
	}
	if !res[1].OK {
		t.Fatalf("healthy vCenter should succeed: %+v", res[1])
	}
	if res[0].Network == "" {
		t.Error("expected a network check summary")
	}
	t.Logf("hung: %q | net: %s | %d sheets in %s", res[0].Error, res[0].Network, len(sheets), time.Since(t0).Round(time.Millisecond))
}

// Cancelling stops everything promptly.
func TestCollectManyCancel(t *testing.T) {
	hung := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(6 * time.Second):
		}
	}))
	defer hung.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, res := CollectMany(ctx, []Target{{Server: hung.Listener.Addr().String(), User: "u", Password: "p", Insecure: true}}, Options{Timeout: time.Minute})
	if time.Since(t0) > 10*time.Second || res[0].OK || res[0].Error != "Cancelled." {
		t.Fatalf("cancel result after %s: %+v", time.Since(t0), res[0])
	}
}

// One unreadable VM (permission fault) must not fail the whole retrieval.
func TestSanitizeMissingSet(t *testing.T) {
	inv := &Inventory{}
	raw := []types.ObjectContent{
		{Obj: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-1"},
			PropSet: []types.DynamicProperty{{Name: "name", Val: "good"}}},
		{Obj: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-2"},
			PropSet:    []types.DynamicProperty{{Name: "name", Val: "locked"}},
			MissingSet: []types.MissingProperty{{Path: "config", Fault: types.LocalizedMethodFault{Fault: &types.NoPermission{}, LocalizedMessage: "Permission to perform this operation was denied."}}}},
	}
	var vms []mo.VirtualMachine
	if err := mo.LoadObjectContent(inv.sanitize(raw, "VM"), &vms); err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 || len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "locked") {
		t.Fatalf("vms=%d warnings=%v", len(vms), inv.Warnings)
	}
}
