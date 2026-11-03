// bcvmtools: a fast, single-binary VMware inventory exporter (RVTools-style).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/vmware/govmomi"
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
		server   = flag.String("s", os.Getenv("VI_SERVER"), "vCenter/ESXi host or URL (env VI_SERVER)")
		user     = flag.String("u", os.Getenv("VI_USER"), "username (env VI_USER)")
		pass     = flag.String("p", "", "password (env VI_PASSWORD; prompts if omitted)")
		insecure = flag.Bool("insecure", true, "skip TLS certificate verification")
		out      = flag.String("o", "", "output path (default bcvmtools_<server>_<timestamp>.xlsx, or a directory for csv)")
		format   = flag.String("f", "xlsx", "output format: xlsx | csv")
		timeout  = flag.Duration("timeout", 10*time.Minute, "overall timeout")
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
	if *server == "" || *user == "" {
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
	inv, sheets, host, tConn, err := CollectAll(ctx, *server, *user, password, *insecure)
	if err != nil {
		fatal(err)
	}
	tCollect := time.Since(start)
	for _, w := range inv.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

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
	fmt.Printf("%d VMs, %d hosts, %d datastores -> %s (connect %s, collect %s, total %s)\n",
		len(inv.VMs), len(inv.Hosts), len(inv.DS), name,
		tConn.Round(time.Millisecond), (tCollect - tConn).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
}

// CollectAll connects, gathers the full inventory and builds the sheets.
// Shared by the CLI and the GUI.
func CollectAll(ctx context.Context, server, user, password string, insecure bool) (inv *Inventory, sheets []*Sheet, host string, tConn time.Duration, err error) {
	var cerr error
	if perr := safely(func() { inv, sheets, host, tConn, cerr = collectAll(ctx, server, user, password, insecure) }); perr != nil {
		return nil, nil, "", 0, perr
	}
	return inv, sheets, host, tConn, cerr
}

func collectAll(ctx context.Context, server, user, password string, insecure bool) (inv *Inventory, sheets []*Sheet, host string, tConn time.Duration, err error) {
	start := time.Now()
	u, err := parseServer(server)
	if err != nil {
		return
	}
	u.User = url.UserPassword(user, password)
	c, err := govmomi.NewClient(ctx, u, insecure)
	if err != nil {
		err = fmt.Errorf("connect: %w", err)
		return
	}
	defer c.Logout(context.Background())
	if t, ok := c.Client.Client.Transport.(*http.Transport); ok {
		t.MaxIdleConnsPerHost = 32
	}
	tConn = time.Since(start)
	if inv, err = Collect(ctx, c.Client); err != nil {
		err = fmt.Errorf("collect: %w", err)
		return
	}
	host = u.Host
	sheets = BuildSheets(inv, host)
	return
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
