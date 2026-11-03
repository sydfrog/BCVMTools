// simserve runs a govmomi vCenter simulator for manual / GUI testing.
//
//	go run ./tools/simserve -vm 20   # prints the host:port to connect to (any credentials work)
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/vmware/govmomi/simulator"
)

func main() {
	vms := flag.Int("vm", 20, "VMs per host")
	addr := flag.String("l", "127.0.0.1:8989", "listen address")
	hang := flag.Bool("hang", false, "act as a vCenter that accepts connections but never answers")
	flag.Parse()
	if *hang {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		fmt.Println("serving", srv.Listener.Addr().String())
		select {}
	}
	m := simulator.VPX()
	m.Machine = *vms
	if err := m.Create(); err != nil {
		log.Fatal(err)
	}
	m.Service.TLS = new(tls.Config) // self-signed HTTPS, like a real vCenter
	s := m.Service.NewServer()
	fmt.Println("serving", s.URL.Host)
	_ = addr
	select {}
}
