package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vim25/soap"
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
	Error    string // short, actionable message
	Details  string // technical error text
	Network  string // DNS/TCP/TLS timings when the connection failed
	About    types.AboutInfo
	VMs      int
	Hosts    int
	Clusters int
	DS       int
	Took     time.Duration
	Warnings []string
}

const maxParallelVCenters = 4

// Options tunes a multi-vCenter collection.
type Options struct {
	Timeout  time.Duration             // budget for each vCenter (connect + collect); default 10 min
	Progress func(label, state string) // optional; called from worker goroutines
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 10 * time.Minute
	}
	return o.Timeout
}

// loginTimeout bounds connecting and logging in: a loaded vCenter can take a minute or more.
const loginTimeout = 2 * time.Minute

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

func isTransient(err error) bool {
	s := strings.ToLower(err.Error())
	for _, k := range []string{"timeout", "deadline exceeded", "eof", "connection reset", "broken pipe", "502", "503", "504", "temporarily"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// collectOne connects to a single vCenter and builds its sheets (each row tagged with the vCenter).
// ctx carries the per-vCenter deadline and the user's Cancel.
func collectOne(ctx context.Context, t Target, o Options) (*Inventory, []*Sheet, error) {
	label := vcLabel(t.Server)
	note := func(s string) {
		if o.Progress != nil {
			o.Progress(label, s)
		}
	}
	u, err := parseServer(t.Server)
	if err != nil {
		return nil, nil, err
	}
	u.User = url.UserPassword(t.User, t.Password)

	note("connecting")
	lctx, lcancel := context.WithTimeout(ctx, loginTimeout)
	c, err := govmomi.NewClient(lctx, u, t.Insecure)
	lcancel()
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		lctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // never hang on logout
		defer cancel()
		c.Logout(lctx)
	}()
	if tr, ok := c.Client.Client.Transport.(*http.Transport); ok {
		tr.MaxIdleConnsPerHost = 32
	}

	note("collecting")
	inv, err := CollectTuned(ctx, c.Client, defaultTuning)
	if err != nil && ctx.Err() == nil && isTransient(err) {
		note("retrying gently (slow vCenter)")
		inv, err = CollectTuned(ctx, c.Client, gentleTuning)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("collect: %w", err)
	}
	note("building tables")
	return inv, BuildSheets(inv, label), nil
}

// CollectMany collects from every target in parallel and merges the results into one set of
// sheets in which every row carries its vCenter. A failing or slow vCenter does not stop the
// others: each has its own deadline, is reported on the vCenters tab, and cancelling ctx stops
// everything promptly. The first sheet returned is the vCenters tab.
func CollectMany(ctx context.Context, targets []Target, opts Options) ([]*Sheet, []ConnResult) {
	results := make([]ConnResult, len(targets))
	per := make([][]*Sheet, len(targets))
	finished := make([]bool, len(targets))
	var mu sync.Mutex
	sem := make(chan struct{}, maxParallelVCenters)
	var wg sync.WaitGroup
	budget := opts.timeout()

	for i, t := range targets {
		i, t := i, t
		results[i] = ConnResult{Label: vcLabel(t.Server), Source: t.Source, User: t.User}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				results[i].Error, results[i].Details, finished[i] = "Cancelled before it started.", "", true
				mu.Unlock()
				return
			}
			defer func() { // a bug while describing a failure must not take the application down
				if r := recover(); r != nil {
					writeCrashLog(r)
					mu.Lock()
					results[i].Error, results[i].Details, finished[i] = fmt.Sprintf("Internal error: %v", r), fmt.Sprint(r), true
					mu.Unlock()
				}
			}()
			start := time.Now()
			vctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			var inv *Inventory
			var sh []*Sheet
			var e error
			if perr := safely(func() { inv, sh, e = collectOne(vctx, t, opts) }); perr != nil {
				e = perr
			}
			r := ConnResult{Label: results[i].Label, Source: t.Source, User: t.User, Took: time.Since(start)}
			if e != nil {
				r.Error, r.Details = friendlyError(e, budget, ctx.Err() != nil)
				if ctx.Err() == nil { // not cancelled: say why we think it failed
					nctx, ncancel := context.WithTimeout(context.Background(), 40*time.Second)
					r.Network = networkCheck(nctx, t.Server)
					ncancel()
				}
			} else {
				r.OK = true
				r.About = inv.About
				r.VMs, r.Hosts, r.Clusters, r.DS = len(inv.VMs), len(inv.Hosts), len(inv.Clusters), len(inv.DS)
				r.Warnings = inv.Warnings
			}
			mu.Lock()
			results[i], per[i], finished[i] = r, sh, true
			mu.Unlock()
			if opts.Progress != nil {
				if r.OK {
					opts.Progress(r.Label, "done")
				} else {
					opts.Progress(r.Label, "failed")
				}
			}
		}()
	}

	// Wait for the workers, but never for longer than budget + grace after the user cancels:
	// a hung vCenter must not be able to freeze the application.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	}

	mu.Lock()
	for i := range results {
		if !finished[i] {
			results[i].Error = "Stopped waiting: this vCenter did not respond in time."
			results[i].Details = "abandoned"
			per[i] = nil
		}
	}
	snapshot := append([]ConnResult(nil), results...)
	perCopy := append([][]*Sheet(nil), per...)
	mu.Unlock()

	merged := MergeSheets(perCopy)
	out := []*Sheet{connectionsSheet(snapshot)}
	out = append(out, merged...)
	if w := warningsSheet(snapshot); w != nil {
		out = append(out, w)
	}
	return out, snapshot
}

// friendlyError turns a raw error into a short, actionable message plus the technical detail.
func friendlyError(err error, budget time.Duration, cancelled bool) (msg, details string) {
	details = err.Error()
	low := strings.ToLower(details)
	for e := err; e != nil; e = errors.Unwrap(e) {
		if !soap.IsVimFault(e) {
			continue
		}
		switch soap.ToVimFault(e).(type) {
		case *types.InvalidLogin:
			return "Login rejected by this vCenter: the user name or password is wrong for it, or the account is locked. Check that the same account works when you log in to this vCenter directly.", details
		case *types.NoPermission:
			return "The account has no permission to read this vCenter's inventory (it needs at least read-only on the root folder).", details
		}
	}
	switch {
	case cancelled || errors.Is(err, context.Canceled):
		return "Cancelled.", details
	case strings.Contains(low, "incorrect user name or password") || strings.Contains(low, "cannot complete login"):
		return "Login rejected by this vCenter: the user name or password is wrong for it, or the account is locked.", details
	case strings.Contains(low, "deadline exceeded") || strings.Contains(low, "timeout") || strings.Contains(low, "timed out"):
		return fmt.Sprintf("Timed out: this vCenter did not answer within the allowed time (limit %s per vCenter, %s to log in). It is probably slow or overloaded; increase the timeout and try again.", budget.Round(time.Second), min(loginTimeout, budget).Round(time.Second)), details
	case strings.Contains(low, "x509") || strings.Contains(low, "certificate"):
		return "The TLS certificate is not trusted. Untick 'Verify TLS certificate' to accept self-signed certificates.", details
	case strings.Contains(low, "no such host"):
		return "The name could not be resolved (DNS). Check the host name or use its IP address.", details
	case strings.Contains(low, "connection refused"):
		return "Connection refused: nothing is listening on that address and port.", details
	case strings.Contains(low, "eof") || strings.Contains(low, "connection reset") || strings.Contains(low, "broken pipe"):
		return "The vCenter dropped the connection while we were collecting (usually overload or a restarting service). Try again.", details
	}
	return details, details
}

// networkCheck gives a quick DNS / TCP / TLS timing summary for a server that failed, to tell a
// slow or unreachable vCenter from a credentials problem.
func networkCheck(ctx context.Context, server string) string {
	u, err := parseServer(server)
	if err != nil {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	var parts []string
	t0 := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "DNS lookup failed: " + err.Error()
	}
	parts = append(parts, fmt.Sprintf("DNS %s (%s)", addrs[0], time.Since(t0).Round(time.Millisecond)))
	t1 := time.Now()
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return strings.Join(parts, "; ") + "; TCP " + port + " failed: " + err.Error()
	}
	parts = append(parts, fmt.Sprintf("TCP %s ok (%s)", port, time.Since(t1).Round(time.Millisecond)))
	t2 := time.Now()
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: host})
	tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return strings.Join(parts, "; ") + "; TLS handshake failed: " + err.Error()
	}
	parts = append(parts, fmt.Sprintf("TLS ok (%s)", time.Since(t2).Round(time.Millisecond)))
	tc.Close()
	return strings.Join(parts, "; ")
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
	s := &Sheet{Name: "vCenters", Header: []string{"vCenter", "Status", "Source", "User", "Product", "Version", "Build", "API version", "Instance UUID", "# VMs", "# Hosts", "# Clusters", "# Datastores", "Collect time", "Warnings", "Error", "Technical details", "Network check"}}
	for _, r := range rs {
		st := "Connected"
		if !r.OK {
			st = "Failed"
		}
		s.Rows = append(s.Rows, []any{r.Label, st, r.Source, r.User, r.About.FullName, r.About.Version, r.About.Build, r.About.ApiVersion, r.About.InstanceUuid,
			r.VMs, r.Hosts, r.Clusters, r.DS, r.Took.Round(time.Millisecond).String(), len(r.Warnings), r.Error, r.Details, r.Network})
	}
	return s
}

// warningsSheet lists every non-fatal problem (VMs or hosts that could not be fully read, and so on).
func warningsSheet(rs []ConnResult) *Sheet {
	s := &Sheet{Name: "Warnings", Header: []string{"vCenter", "Message"}}
	for _, r := range rs {
		for _, w := range r.Warnings {
			s.Rows = append(s.Rows, []any{r.Label, w})
		}
	}
	if len(s.Rows) == 0 {
		return nil
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

// DiscoverVCFOps asks VCF Operations (the VCF 9.x successor of Aria/vRealize Operations) which
// vCenters it monitors. It uses the Operations Suite API: acquire a token, then list the
// VMWARE adapter instances, whose "VCURL" identifier is the vCenter address.
// authSource is optional (blank = a local Operations user).
func DiscoverVCFOps(ctx context.Context, host, user, pass, authSource string, insecure bool) ([]VCFVCenter, error) {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	base := strings.TrimRight(host, "/")
	cl := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	defer cl.CloseIdleConnections()

	login := map[string]string{"username": user, "password": pass}
	if strings.TrimSpace(authSource) != "" {
		login["authSource"] = strings.TrimSpace(authSource)
	}
	body, _ := json.Marshal(login)
	var tok struct {
		Token string `json:"token"`
	}
	if err := vcfDoAuth(ctx, cl, "POST", base+"/suite-api/api/auth/token/acquire", "", body, &tok); err != nil {
		return nil, fmt.Errorf("VCF Operations login failed: %w", err)
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("VCF Operations login returned no token")
	}
	auth := "OpsToken " + tok.Token

	type ident struct {
		IdentifierType struct {
			Name string `json:"name"`
		} `json:"identifierType"`
		Value string `json:"value"`
	}
	type key struct {
		Name                string  `json:"name"`
		ResourceIdentifiers []ident `json:"resourceIdentifiers"`
	}
	var adapters struct {
		Instances []struct {
			ResourceKey key `json:"resourceKey"`
		} `json:"adapterInstancesInfoDto"`
	}
	if err := vcfDoAuth(ctx, cl, "GET", base+"/suite-api/api/adapters?adapterKindKey=VMWARE", auth, nil, &adapters); err != nil {
		return nil, fmt.Errorf("could not list vCenter adapters: %w", err)
	}
	seen := map[string]bool{}
	var out []VCFVCenter
	for _, a := range adapters.Instances {
		addr := ""
		for _, id := range a.ResourceKey.ResourceIdentifiers {
			if strings.EqualFold(id.IdentifierType.Name, "VCURL") && strings.TrimSpace(id.Value) != "" {
				addr = strings.TrimSpace(id.Value)
			}
		}
		if addr == "" {
			continue
		}
		// the value may be a bare host name or a URL such as https://vc.corp/sdk
		if strings.Contains(addr, "://") {
			if u, err := url.Parse(addr); err == nil && u.Host != "" {
				addr = u.Host
			}
		}
		if k := strings.ToLower(addr); !seen[k] {
			seen[k] = true
			out = append(out, VCFVCenter{FQDN: addr, Domain: a.ResourceKey.Name})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("VCF Operations has no vCenter adapter instances (or this user cannot see them)")
	}
	return out, nil
}

// vcfDoAuth is vcfDo with a complete Authorization header value.
func vcfDoAuth(ctx context.Context, cl *http.Client, method, u, authHeader string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
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
