package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
)

// Regression: a failing vCenter must be reported (the GUI once crashed when an error was
// swallowed and an empty result was shown) and must not stop the app.
func TestCollectManyReportsErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sheets, res := CollectMany(ctx, []Target{{Server: "127.0.0.1:1", User: "u", Password: "p", Insecure: true}})
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
	})
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
