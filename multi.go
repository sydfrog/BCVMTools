package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vim25/types"
)

// Target is one vCenter (or ESXi host) to collect from.
type Target struct {
	Server   string
	User     string
	Password string
	Source   string // "Manual" or "VCF: <sddc> / <domain>"
	Insecure bool
}

// ConnResult describes the outcome of connecting to one Target (shown on the vCenters tab).
type ConnResult struct {
	Label    string // host name used in the vCenter column
	Source   string
	User     string
	OK       bool
	Error    string
	About    types.AboutInfo
	VMs      int
	Hosts    int
	Clusters int
	DS       int
	Took     time.Duration
	Warnings []string
}

const maxParallelVCenters = 4

// vcLabel is the name that identifies a vCenter in the "vCenter" column.
func vcLabel(server string) string {
	u, err := parseServer(server)
	if err != nil || u.Host == "" {
		return strings.TrimSpace(server)
	}
	if p := u.Port(); p == "" || p == "443" {
		return u.Hostname()
	}
	return u.Host
}

// collectOne connects to a single vCenter and builds its sheets (each row tagged with the vCenter).
func collectOne(ctx context.Context, t Target) (inv *Inventory, sheets []*Sheet, err error) {
	u, err := parseServer(t.Server)
	if err != nil {
		return nil, nil, err
	}
	u.User = url.UserPassword(t.User, t.Password)
	c, err := govmomi.NewClient(ctx, u, t.Insecure)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	defer c.Logout(context.Background())
	if tr, ok := c.Client.Client.Transport.(*http.Transport); ok {
		tr.MaxIdleConnsPerHost = 32
	}
	if inv, err = Collect(ctx, c.Client); err != nil {
		return nil, nil, fmt.Errorf("collect: %w", err)
	}
	return inv, BuildSheets(inv, vcLabel(t.Server)), nil
}

// CollectMany collects from every target in parallel and merges the results into one set of
// sheets in which every row carries its vCenter. A failing vCenter does not stop the others;
// it is reported on the vCenters tab. The first sheet returned is that vCenters tab.
func CollectMany(ctx context.Context, targets []Target) ([]*Sheet, []ConnResult) {
	results := make([]ConnResult, len(targets))
	per := make([][]*Sheet, len(targets))
	sem := make(chan struct{}, maxParallelVCenters)
	var wg sync.WaitGroup
	for i, t := range targets {
		i, t := i, t
		results[i] = ConnResult{Label: vcLabel(t.Server), Source: t.Source, User: t.User}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			var inv *Inventory
			var sh []*Sheet
			var e error
			if perr := safely(func() { inv, sh, e = collectOne(ctx, t) }); perr != nil {
				e = perr
			}
			results[i].Took = time.Since(start)
			if e != nil {
				results[i].Error = e.Error()
				return
			}
			results[i].OK = true
			results[i].About = inv.About
			results[i].VMs, results[i].Hosts, results[i].Clusters, results[i].DS = len(inv.VMs), len(inv.Hosts), len(inv.Clusters), len(inv.DS)
			results[i].Warnings = inv.Warnings
			per[i] = sh
		}()
	}
	wg.Wait()
	merged := MergeSheets(per)
	return append([]*Sheet{connectionsSheet(results)}, merged...), results
}

// MergeSheets concatenates same-named sheets from several vCenters, keeping first-seen order.
func MergeSheets(per [][]*Sheet) []*Sheet {
	var out []*Sheet
	idx := map[string]*Sheet{}
	for _, list := range per {
		for _, s := range list {
			if m, ok := idx[s.Name]; ok {
				m.Rows = append(m.Rows, s.Rows...)
				continue
			}
			c := &Sheet{Name: s.Name, Header: s.Header, Rows: append([][]any(nil), s.Rows...)}
			idx[s.Name] = c
			out = append(out, c)
		}
	}
	return out
}

// tagVCenter inserts the leading "vCenter" column into a sheet.
func tagVCenter(s *Sheet, label string) {
	s.Header = append([]string{"vCenter"}, s.Header...)
	for i, r := range s.Rows {
		s.Rows[i] = append([]any{label}, r...)
	}
}

func connectionsSheet(rs []ConnResult) *Sheet {
	s := &Sheet{Name: "vCenters", Header: []string{"vCenter", "Status", "Source", "User", "Product", "Version", "Build", "API version", "Instance UUID", "# VMs", "# Hosts", "# Clusters", "# Datastores", "Collect time", "Warnings", "Error"}}
	for _, r := range rs {
		st := "Connected"
		if !r.OK {
			st = "Failed"
		}
		s.Rows = append(s.Rows, []any{r.Label, st, r.Source, r.User, r.About.FullName, r.About.Version, r.About.Build, r.About.ApiVersion, r.About.InstanceUuid,
			r.VMs, r.Hosts, r.Clusters, r.DS, r.Took.Round(time.Millisecond).String(), len(r.Warnings), r.Error})
	}
	return s
}

// ---- VMware Cloud Foundation (SDDC Manager) discovery -------------------------------------

// VCFVCenter is a vCenter registered with an SDDC Manager.
type VCFVCenter struct {
	FQDN    string
	Domain  string
	Version string
}

// DiscoverVCF asks an SDDC Manager for its vCenters (POST /v1/tokens, then GET /v1/vcenters,
// falling back to GET /v1/domains).
func DiscoverVCF(ctx context.Context, host, user, pass string, insecure bool) ([]VCFVCenter, error) {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	base := strings.TrimRight(host, "/")
	cl := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	defer cl.CloseIdleConnections()

	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	var tok struct {
		AccessToken string `json:"accessToken"`
	}
	if err := vcfDo(ctx, cl, "POST", base+"/v1/tokens", "", body, &tok); err != nil {
		return nil, fmt.Errorf("SDDC Manager login failed: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("SDDC Manager login returned no access token")
	}

	var vcs struct {
		Elements []struct {
			FQDN    string `json:"fqdn"`
			Version string `json:"version"`
			Domain  struct {
				Name string `json:"name"`
			} `json:"domain"`
		} `json:"elements"`
	}
	var out []VCFVCenter
	if err := vcfDo(ctx, cl, "GET", base+"/v1/vcenters", tok.AccessToken, nil, &vcs); err == nil {
		for _, e := range vcs.Elements {
			if e.FQDN != "" {
				out = append(out, VCFVCenter{FQDN: e.FQDN, Domain: e.Domain.Name, Version: e.Version})
			}
		}
	}
	if len(out) == 0 {
		var doms struct {
			Elements []struct {
				Name     string `json:"name"`
				VCenters []struct {
					FQDN string `json:"fqdn"`
				} `json:"vcenters"`
			} `json:"elements"`
		}
		if err := vcfDo(ctx, cl, "GET", base+"/v1/domains", tok.AccessToken, nil, &doms); err != nil {
			return nil, fmt.Errorf("could not list vCenters: %w", err)
		}
		for _, d := range doms.Elements {
			for _, v := range d.VCenters {
				if v.FQDN != "" {
					out = append(out, VCFVCenter{FQDN: v.FQDN, Domain: d.Name})
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SDDC Manager reported no vCenters")
	}
	return out, nil
}

func vcfDo(ctx context.Context, cl *http.Client, method, u, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s %s: HTTP %d %s", method, u, resp.StatusCode, msg)
	}
	return json.Unmarshal(data, out)
}
