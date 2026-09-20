package main

import (
	"fmt"
	"net"
	"strings"
)

// checkListenAddrs warns when an advertised listen address is not configured
// on any local interface. That happens on a fresh host when the container
// network was created without the fixed subnet, and the symptom is a reverse
// proxy that simply cannot reach shipd.
func checkListenAddrs(addrs []string) error {
	if len(addrs) == 0 {
		return nil
	}
	local := map[string]bool{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil // cannot check; not fatal
	}
	for _, ifc := range ifaces {
		as, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
			}
		}
	}
	var missing []string
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a
		}
		if !local[host] {
			missing = append(missing, a)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("listen_extra address(es) not present on any interface: %s (create the container network with the expected subnet, or bind shipd to an address that exists here)",
			strings.Join(missing, ", "))
	}
	return nil
}
