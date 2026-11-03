// bcvmtools: a fast, single-binary VMware inventory exporter (RVTools-style).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/term"
)

var version = "dev"

func main() {
	defer func() {
		if r := recover(); r != nil {
			writeCrashLog(r)
			panic(r)
		}
	}()
	var (
		server   = flag.String("s", os.Getenv("VI_SERVER"), "vCenter/ESXi host(s) or URL(s), comma separated (env VI_SERVER)")
		user     = flag.String("u", os.Getenv("VI_USER"), "username (env VI_USER)")
		pass     = flag.String("p", "", "password (env VI_PASSWORD; prompts if omitted)")
		insecure = flag.Bool("insecure", true, "skip TLS certificate verification")
		out      = flag.String("o", "", "output path (default bcvmtools_<server>_<timestamp>.xlsx, or a directory for csv)")
		format   = flag.String("f", "xlsx", "output format: xlsx | csv")
		timeout  = flag.Duration("timeout", 10*time.Minute, "overall timeout")
		vcf      = flag.String("vcf", "", "VCF SDDC Manager host: discover and collect from all its vCenters (uses -u/-p)")
		vcUser   = flag.String("vcuser", "", "user for the discovered vCenters (default: same as -u)")
		vcPass   = flag.String("vcpass", "", "password for the discovered vCenters (default: same as -p)")
		showVer  = flag.Bool("version", false, "print version")
	)
	gui := flag.Bool("gui", false, "open the graphical interface (default when started without arguments on Windows)")
	attachConsole()
	if len(os.Args) == 1 {
		*gui = true
	}
	flag.Parse()
	if *gui {
		if err := runGUI(); err != nil {
			fatal(err)
		}
		return
	}
	if *showVer {
		fmt.Println("bcvmtools", version)
		return
	}
	if (*server == "" && *vcf == "") || *user == "" {
		flag.Usage()
		os.Exit(2)
	}
	password := *pass
	if password == "" {
		password = os.Getenv("VI_PASSWORD")
	}
	if password == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fatal(err)
		}
		password = string(b)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	start := time.Now()
	var targets []Target
	for _, s := range strings.Split(*server, ",") {
		if s = strings.TrimSpace(s); s != "" {
			targets = append(targets, Target{Server: s, User: *user, Password: password, Source: "Manual", Insecure: *insecure})
		}
	}
	if *vcf != "" {
		vcs, err := DiscoverVCF(ctx, *vcf, *user, password, *insecure)
		if err != nil {
			fatal(err)
		}
		vu, vp := firstNonEmpty(*vcUser, *user), firstNonEmpty(*vcPass, password)
		for _, v := range vcs {
			targets = append(targets, Target{Server: v.FQDN, User: vu, Password: vp, Source: "VCF: " + *vcf + " / " + v.Domain, Insecure: *insecure})
		}
		fmt.Fprintf(os.Stderr, "VCF %s: found %d vCenter(s)\n", *vcf, len(vcs))
	}
	targets = dedupeTargets(targets)
	sheets, results := CollectMany(ctx, targets)
	ok, vms, hosts := 0, 0, 0
	for _, r := range results {
		if r.OK {
			ok++
			vms += r.VMs
			hosts += r.Hosts
		} else {
			fmt.Fprintf(os.Stderr, "FAILED %s: %s\n", r.Label, r.Error)
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(os.Stderr, "warning (%s): %s\n", r.Label, w)
		}
	}
	if ok == 0 {
		fatal(fmt.Errorf("could not collect from any vCenter"))
	}
	host := results[0].Label
	if len(results) > 1 {
		host = fmt.Sprintf("%d-vcenters", len(results))
	}
	var err error

	name := *out
	if name == "" {
		name = fmt.Sprintf("bcvmtools_%s_%s", strings.NewReplacer(":", "_").Replace(host), time.Now().Format("20060102_150405"))
		if *format == "xlsx" {
			name += ".xlsx"
		}
	}
	switch *format {
	case "xlsx":
		err = WriteXLSX(name, sheets)
	case "csv":
		err = WriteCSV(name, sheets)
	default:
		err = fmt.Errorf("unknown format %q", *format)
	}
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%d/%d vCenters, %d VMs, %d hosts -> %s (total %s)\n", ok, len(results), vms, hosts, name, time.Since(start).Round(time.Millisecond))
}

// dedupeTargets drops repeated servers (same vCenter given twice, or found by VCF and also typed).
func dedupeTargets(in []Target) []Target {
	seen := map[string]bool{}
	var out []Target
	for _, t := range in {
		k := strings.ToLower(vcLabel(t.Server))
		if !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func parseServer(s string) (*url.URL, error) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	return u, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// writeCrashLog appends a panic with its stack to <user config dir>/BCVMTools/crash.log
// (the GUI build has no console, so this is the only trace of an unexpected failure).
func writeCrashLog(v any) string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(d, "BCVMTools", "crash.log")
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return ""
	}
	defer f.Close()
	fmt.Fprintf(f, "---- %s\n%v\n%s\n", time.Now().Format(time.RFC3339), v, debug.Stack())
	return p
}

// safely runs fn and converts a panic into an error (and a crash-log entry).
func safely(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			p := writeCrashLog(r)
			err = fmt.Errorf("internal error: %v (details in %s)", r, p)
		}
	}()
	fn()
	return nil
}

// appDataDir returns <user config dir>\BCVMTools (e.g. %AppData%\BCVMTools), creating it if
// it does not exist, and proves it is writable. The error says exactly what is wrong.
func appDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the user configuration folder (%%AppData%%): %w", err)
	}
	dir := filepath.Join(base, "BCVMTools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create the folder %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return "", fmt.Errorf("the folder %s exists but you do not have permission to write to it: %w", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return dir, nil
}

// captureFatalCrashes makes the Go runtime write fatal errors / unrecovered panics
// (from any goroutine) to crash.log; the GUI build has no console to show them on.
func captureFatalCrashes(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "---- session started %s\n", time.Now().Format(time.RFC3339))
	debug.SetCrashOutput(f, debug.CrashOptions{})
}
