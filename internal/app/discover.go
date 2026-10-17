package app

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// Finding a printer without knowing its address. Klipper printers answer on
// port 7125, so the scan tries that port on every address in this machine's
// own local networks, then asks whatever answered whether it really is
// Moonraker. A /24 is 254 addresses; with a short timeout and a few hundred at
// once it takes about a second.
//
// It only ever looks at private networks this machine is directly on. It never
// scans anything on the internet, and only runs when someone presses the
// button.

const moonrakerPort = 7125

// localSubnets is every private IPv4 /24 this machine has an address in.
// A bigger network (a /16 at home is rare, but happens) is cut to the /24
// around this machine's own address: that is where the printer almost always
// is, and 65 000 probes is not a button press.
func localSubnets() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP.To4())
		if !ok || !ip.IsPrivate() || ip.IsLoopback() {
			continue
		}
		p, err := ip.Prefix(24)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// hostsIn lists the usable addresses of a /24 (or smaller).
func hostsIn(p netip.Prefix) []string {
	var out []string
	for a := p.Masked().Addr().Next(); p.Contains(a); a = a.Next() {
		if b := a.As4(); b[3] == 255 {
			break
		}
		out = append(out, a.String())
	}
	return out
}

// scanForMoonraker tries the port on every host and asks each that is open.
func scanForMoonraker(ctx context.Context, hosts []string, port int) []Found {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var mu sync.Mutex
	var found []Found
	sem := make(chan struct{}, 128)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			addr := net.JoinHostPort(h, fmt.Sprint(port))
			d := net.Dialer{Timeout: 400 * time.Millisecond}
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return
			}
			conn.Close()
			if f, ok := probeMoonraker(ctx, "http://"+addr); ok {
				mu.Lock()
				found = append(found, f)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].Address < found[j].Address })
	return found
}

// checkAddress tries what someone typed: as given, and then on Moonraker's
// port if no port was given. People type the address they open Fluidd at,
// which is port 80 on most printers and proxies Moonraker anyway.
func checkAddress(ctx context.Context, raw string) (Found, bool) {
	base, err := normaliseLinkURL(raw)
	if err != nil {
		return Found{}, false
	}
	if f, ok := probeMoonraker(ctx, base); ok {
		return f, true
	}
	host := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	if _, _, err := net.SplitHostPort(host); err != nil {
		return probeMoonraker(ctx, fmt.Sprintf("http://%s:%d", host, moonrakerPort))
	}
	return Found{}, false
}
