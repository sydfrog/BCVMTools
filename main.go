// bcvmtools: a fast, single-binary VMware inventory exporter (RVTools-style).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vmware/govmomi"
	"golang.org/x/term"
)

var version = "dev"

func main() {
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
	flag.Parse()
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
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fatal(err)
		}
		password = string(b)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	start := time.Now()

	u, err := parseServer(*server)
	if err != nil {
		fatal(err)
	}
	u.User = url.UserPassword(*user, password)
	c, err := govmomi.NewClient(ctx, u, *insecure)
	if err != nil {
		fatal(fmt.Errorf("connect: %w", err))
	}
	defer c.Logout(context.Background())
	if t, ok := c.Client.Client.Transport.(*http.Transport); ok {
		t.MaxIdleConnsPerHost = 32
	}
	tConn := time.Since(start)

	inv, err := Collect(ctx, c.Client)
	if err != nil {
		fatal(fmt.Errorf("collect: %w", err))
	}
	tCollect := time.Since(start)
	for _, w := range inv.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	sheets := BuildSheets(inv, u.Host)
	name := *out
	if name == "" {
		name = fmt.Sprintf("bcvmtools_%s_%s", strings.NewReplacer(":", "_").Replace(u.Host), time.Now().Format("20060102_150405"))
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
