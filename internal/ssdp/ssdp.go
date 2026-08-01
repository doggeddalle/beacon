// Package ssdp implements the SSDP (Simple Service Discovery Protocol) side of
// UPnP: it announces the MediaServer on the local network and answers the
// M-SEARCH queries that control points (TVs, VLC, phones) broadcast to find
// servers.
//
// Discovery is the part of DLNA that fails most often on a NAS, and almost
// always for one of two reasons. Both are handled here:
//
//   - Multi-homing. A NAS commonly has link aggregation, VLANs, or Docker
//     bridges. Joining the multicast group on only the kernel's default
//     interface leaves clients on every other subnet unable to see the server.
//     Beacon joins on every eligible interface and answers each search with the
//     address of the interface it arrived on.
//   - Address changes. After a DHCP renewal the advertised LOCATION points at an
//     address the host no longer owns. Beacon watches for address changes,
//     increments BOOTID so control points invalidate what they cached, and
//     re-announces.
package ssdp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"

	"beacon/internal/netutil"
)

const (
	multicastAddr = "239.255.255.250:1900"
	ssdpPort      = 1900
	maxAge        = 1800 // seconds; clients re-discover before this expires
	// multicastTTL of 4 is the conventional UPnP value: enough for a segment
	// with a couple of hops, not enough to leak off the LAN.
	multicastTTL = 4
	// aliveBurst repeats each announcement, since SSDP runs over unreliable UDP
	// and a single lost datagram means a TV never learns the server exists.
	aliveBurst = 3
	// maxPendingReplies bounds the goroutines waiting out their MX jitter, so a
	// burst of searches cannot pile up unboundedly.
	maxPendingReplies = 64
	// addrCheckInterval is how often the interface addresses are re-examined.
	addrCheckInterval = 30 * time.Second
)

// serverString is the SERVER header. Set by New from the running version.
var defaultServerString = "Linux/1.0 UPnP/1.0 Beacon/dev"

// target is one advertised (NT/ST, USN) pair.
type target struct {
	nt  string
	usn string
}

// Server advertises a single UPnP root device over SSDP.
type Server struct {
	udn        string // "uuid:...."
	deviceType string
	services   []string
	// locationPath is the device description path; the host part is filled in
	// per interface so each client is told an address it can actually reach.
	locationPath string
	httpPort     int
	ifaceFilter  string
	serverString string
	log          *slog.Logger

	group   *net.UDPAddr
	targets []target

	// bootID must change whenever the device's addresses change, or a control
	// point that cached a stale LOCATION never re-fetches the description.
	bootID atomic.Int64

	mu       sync.Mutex
	joined   map[int]bool // interface index -> group joined
	addrSeen string       // fingerprint of the current interface addresses

	pending chan struct{} // bounds in-flight delayed replies
}

// Config configures the SSDP server.
type Config struct {
	UDN        string
	DeviceType string
	Services   []string
	// LocationPath is the device description path, e.g. "/rootDesc.xml".
	LocationPath string
	// HTTPPort is the port the media server is listening on.
	HTTPPort int
	// Interface optionally restricts SSDP to one interface, by name or IP.
	Interface string
	// ServerString is the SERVER header value; a default is used when empty.
	ServerString string
	Logger       *slog.Logger
}

// New builds an SSDP server.
func New(cfg Config) (*Server, error) {
	group, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return nil, err
	}
	srvString := cfg.ServerString
	if srvString == "" {
		srvString = defaultServerString
	}
	s := &Server{
		udn:          cfg.UDN,
		deviceType:   cfg.DeviceType,
		services:     cfg.Services,
		locationPath: cfg.LocationPath,
		httpPort:     cfg.HTTPPort,
		ifaceFilter:  cfg.Interface,
		serverString: srvString,
		log:          cfg.Logger,
		group:        group,
		joined:       map[int]bool{},
		pending:      make(chan struct{}, maxPendingReplies),
	}
	// Seconds since the epoch is monotonic across reboots and fits the spec's
	// 31-bit range, so every restart yields a higher BOOTID than the last.
	s.bootID.Store(time.Now().Unix() & 0x7fffffff)
	s.targets = s.buildTargets()
	return s, nil
}

// buildTargets returns every (NT, USN) the device advertises.
func (s *Server) buildTargets() []target {
	ts := []target{
		{"upnp:rootdevice", s.udn + "::upnp:rootdevice"},
		{s.udn, s.udn},
		{s.deviceType, s.udn + "::" + s.deviceType},
	}
	for _, svc := range s.services {
		ts = append(ts, target{svc, s.udn + "::" + svc})
	}
	return ts
}

// Run listens for M-SEARCH requests and periodically re-announces the device
// until ctx is cancelled, then sends byebye notifications.
func (s *Server) Run(ctx context.Context) error {
	// Bind the wildcard address rather than the group, so unicast M-SEARCH sent
	// straight to the device — which UPnP 1.1 allows and some control points use
	// for re-discovery — is received as well as multicast.
	lc := net.ListenConfig{Control: reusePort}
	pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(ssdpPort))
	if err != nil {
		return fmt.Errorf("ssdp listen: %w (is another UPnP/DLNA server already using port %d?)", err, ssdpPort)
	}
	conn := ipv4.NewPacketConn(pc)
	defer conn.Close()

	// FlagInterface tells us which interface each datagram arrived on, which is
	// how a reply can advertise an address the requester can actually route to.
	if err := conn.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		s.log.Debug("ssdp: per-packet interface info unavailable", "err", err)
	}
	_ = conn.SetMulticastTTL(multicastTTL)
	_ = conn.SetMulticastLoopback(false)

	n := s.refreshMemberships(conn)
	if n == 0 {
		return fmt.Errorf("ssdp: could not join %s on any interface", multicastAddr)
	}
	s.log.Info("ssdp listening", "group", multicastAddr, "interfaces", n, "boot_id", s.bootID.Load())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.announceLoop(ctx, conn) }()
	go func() { defer wg.Done(); s.watchAddresses(ctx, conn) }()

	// Unblock the read below on cancellation.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			pc.SetReadDeadline(time.Now())
		case <-stopped:
		}
	}()

	err = s.readLoop(ctx, conn)
	close(stopped)
	wg.Wait()

	// Always say goodbye: without it clients keep a dead entry for up to max-age.
	s.byebye()
	return err
}

// readLoop answers searches until the context is cancelled.
func (s *Server) readLoop(ctx context.Context, conn *ipv4.PacketConn) error {
	buf := make([]byte, 2048)
	for {
		n, cm, src, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A transient read error must not kill discovery for the process
			// lifetime; pause briefly and carry on.
			s.log.Warn("ssdp read error", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		udpSrc, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		ifIndex := 0
		if cm != nil {
			ifIndex = cm.IfIndex
		}
		s.handlePacket(ctx, conn, string(buf[:n]), udpSrc, ifIndex)
	}
}

// handlePacket parses a datagram and, if it is a matching M-SEARCH, schedules a
// reply.
func (s *Server) handlePacket(ctx context.Context, conn *ipv4.PacketConn, req string, src *net.UDPAddr, ifIndex int) {
	if !strings.HasPrefix(req, "M-SEARCH") {
		return
	}
	// MAN must be exactly "ssdp:discover"; anything else is not a search for us.
	if !strings.EqualFold(headerValue(req, "MAN"), "ssdp:discover") {
		return
	}
	st := headerValue(req, "ST")
	if st == "" {
		return
	}

	var matches []target
	for _, t := range s.targets {
		if st == "ssdp:all" || st == t.nt {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return
	}

	location := s.locationFor(ifIndex)
	if location == "" {
		return // no usable address on that interface
	}

	// UPnP requires the response to be delayed by a random interval in [0, MX]
	// so a room full of devices does not answer a broadcast in unison. The old
	// code claimed to do this in a comment and replied immediately.
	delay := jitter(headerValue(req, "MX"))

	select {
	case s.pending <- struct{}{}:
	default:
		return // too many searches in flight; dropping one is what MX protects
	}
	go func() {
		defer func() { <-s.pending }()
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		for _, t := range matches {
			s.respond(conn, src, st, t, location)
		}
	}()
}

// jitter returns a random delay within the requested MX window.
func jitter(mx string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(mx))
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > 5 { // the spec caps MX at 5; ignore larger claims
		secs = 5
	}
	return time.Duration(rand.Int63n(int64(secs) * int64(time.Second)))
}

// locationFor builds the device description URL using the address of the
// interface a request arrived on, falling back to the primary address.
func (s *Server) locationFor(ifIndex int) string {
	if ifIndex > 0 {
		if ni, err := net.InterfaceByIndex(ifIndex); err == nil {
			if ip := netutil.InterfaceIPv4(ni); ip != nil {
				return s.urlFor(ip)
			}
		}
	}
	ip, err := netutil.PrimaryIPv4(s.ifaceFilter)
	if err != nil {
		return ""
	}
	return s.urlFor(ip)
}

func (s *Server) urlFor(ip net.IP) string {
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(s.httpPort)) + s.locationPath
}

// respond unicasts a search response for one target back to the requester.
func (s *Server) respond(conn *ipv4.PacketConn, dst *net.UDPAddr, st string, t target, location string) {
	msg := s.searchResponse(st, t, location)
	if _, err := conn.WriteTo([]byte(msg), nil, dst); err != nil {
		s.log.Debug("ssdp respond failed", "dst", dst.String(), "err", err)
	}
}

// searchResponse builds the M-SEARCH reply for one target.
func (s *Server) searchResponse(st string, t target, location string) string {
	// For a specific ST the response echoes that ST; for ssdp:all we use the
	// target's own NT as the ST.
	respST := st
	if st == "ssdp:all" {
		respST = t.nt
	}
	return "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=" + strconv.Itoa(maxAge) + "\r\n" +
		// http.TimeFormat renders the zone as GMT, which is what HTTP-date
		// requires; time.RFC1123 renders "UTC" and is not a valid HTTP-date.
		"DATE: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n" +
		"EXT:\r\n" +
		"LOCATION: " + location + "\r\n" +
		"SERVER: " + s.serverString + "\r\n" +
		"ST: " + respST + "\r\n" +
		"USN: " + t.usn + "\r\n" +
		"BOOTID.UPNP.ORG: " + strconv.FormatInt(s.bootID.Load(), 10) + "\r\n" +
		"CONFIGID.UPNP.ORG: 1\r\n\r\n"
}

// announceLoop sends the initial alive burst then re-announces periodically.
func (s *Server) announceLoop(ctx context.Context, conn *ipv4.PacketConn) {
	s.announce(conn)
	ticker := time.NewTicker((maxAge / 3) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.announce(conn)
		}
	}
}

// announce multicasts ssdp:alive NOTIFY for every target, on every interface,
// with that interface's own address in LOCATION.
func (s *Server) announce(conn *ipv4.PacketConn) {
	ifaces, err := netutil.MulticastInterfaces(s.ifaceFilter)
	if err != nil {
		s.log.Debug("ssdp announce: no interfaces", "err", err)
		return
	}
	boot := strconv.FormatInt(s.bootID.Load(), 10)

	for i := range ifaces {
		ni := ifaces[i]
		ip := netutil.InterfaceIPv4(&ni)
		if ip == nil {
			continue
		}
		if err := conn.SetMulticastInterface(&ni); err != nil {
			s.log.Debug("ssdp: cannot send on interface", "iface", ni.Name, "err", err)
			continue
		}
		location := s.urlFor(ip)
		for range aliveBurst {
			for _, t := range s.targets {
				msg := "NOTIFY * HTTP/1.1\r\n" +
					"HOST: " + multicastAddr + "\r\n" +
					"CACHE-CONTROL: max-age=" + strconv.Itoa(maxAge) + "\r\n" +
					"LOCATION: " + location + "\r\n" +
					"NT: " + t.nt + "\r\n" +
					"NTS: ssdp:alive\r\n" +
					"SERVER: " + s.serverString + "\r\n" +
					"USN: " + t.usn + "\r\n" +
					"BOOTID.UPNP.ORG: " + boot + "\r\n" +
					"CONFIGID.UPNP.ORG: 1\r\n\r\n"
				_, _ = conn.WriteTo([]byte(msg), nil, s.group)
			}
			time.Sleep(50 * time.Millisecond) // space the burst out a little
		}
	}
}

// refreshMemberships joins the multicast group on every eligible interface,
// returning how many are currently joined.
func (s *Server) refreshMemberships(conn *ipv4.PacketConn) int {
	ifaces, err := netutil.MulticastInterfaces(s.ifaceFilter)
	if err != nil {
		s.log.Warn("ssdp: no usable multicast interface", "err", err)
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	live := map[int]bool{}
	for i := range ifaces {
		ni := ifaces[i]
		live[ni.Index] = true
		if s.joined[ni.Index] {
			continue
		}
		if err := conn.JoinGroup(&ni, s.group); err != nil {
			s.log.Debug("ssdp: join failed", "iface", ni.Name, "err", err)
			continue
		}
		s.joined[ni.Index] = true
		s.log.Info("ssdp joined multicast group", "iface", ni.Name, "ip", netutil.InterfaceIPv4(&ni))
	}
	// Drop memberships for interfaces that have gone away.
	for idx := range s.joined {
		if !live[idx] {
			delete(s.joined, idx)
		}
	}
	return len(s.joined)
}

// watchAddresses re-joins groups and re-announces when the host's addresses
// change — the DHCP-renewal case that otherwise requires a manual restart.
func (s *Server) watchAddresses(ctx context.Context, conn *ipv4.PacketConn) {
	s.mu.Lock()
	s.addrSeen = addressFingerprint(s.ifaceFilter)
	s.mu.Unlock()

	t := time.NewTicker(addrCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		current := addressFingerprint(s.ifaceFilter)
		s.mu.Lock()
		changed := current != s.addrSeen
		s.addrSeen = current
		s.mu.Unlock()
		if !changed {
			continue
		}

		// A new BOOTID is the signal that makes control points discard the
		// LOCATION they cached against the old address.
		boot := s.bootID.Add(1)
		s.log.Info("network addresses changed — re-announcing", "boot_id", boot)
		s.byebye()
		s.refreshMemberships(conn)
		s.announce(conn)
	}
}

// addressFingerprint is a stable string describing the current IPv4 addresses.
func addressFingerprint(filter string) string {
	ifaces, err := netutil.MulticastInterfaces(filter)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i := range ifaces {
		ni := ifaces[i]
		if ip := netutil.InterfaceIPv4(&ni); ip != nil {
			b.WriteString(ni.Name)
			b.WriteByte('=')
			b.WriteString(ip.String())
			b.WriteByte(';')
		}
	}
	return b.String()
}

// byebye multicasts ssdp:byebye for every target on a short-lived socket.
func (s *Server) byebye() {
	conn, err := net.DialUDP("udp4", nil, s.group)
	if err != nil {
		return
	}
	defer conn.Close()
	boot := strconv.FormatInt(s.bootID.Load(), 10)
	for _, t := range s.targets {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + multicastAddr + "\r\n" +
			"NT: " + t.nt + "\r\n" +
			"NTS: ssdp:byebye\r\n" +
			"USN: " + t.usn + "\r\n" +
			"BOOTID.UPNP.ORG: " + boot + "\r\n" +
			"CONFIGID.UPNP.ORG: 1\r\n\r\n"
		_, _ = conn.Write([]byte(msg))
	}
	s.log.Info("ssdp sent byebye")
}

// headerValue extracts a header value from a raw HTTP-over-UDP message
// (case-insensitive name match).
func headerValue(msg, name string) string {
	sc := bufio.NewScanner(strings.NewReader(msg))
	lname := strings.ToLower(name)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(line[:i])) == lname {
			return strings.Trim(strings.TrimSpace(line[i+1:]), `"`)
		}
	}
	return ""
}
