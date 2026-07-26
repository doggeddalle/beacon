// Package ssdp implements the SSDP (Simple Service Discovery Protocol) side of
// UPnP: it announces the MediaServer on the local network and answers the
// M-SEARCH queries that control points (TVs, VLC, phones) broadcast to find
// servers.
package ssdp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

const (
	multicastAddr = "239.255.255.250:1900"
	maxAge        = 1800 // seconds; clients re-discover before this expires
	serverString  = "Linux/ARM UPnP/1.0 Beacon/0.1"
)

// Target is one advertised (NT/ST, USN) pair.
type target struct {
	nt  string
	usn string
}

// Server advertises a single UPnP root device over SSDP.
type Server struct {
	udn        string // "uuid:...."
	deviceType string
	services   []string
	location   string // http://ip:port/rootDesc.xml
	log        *slog.Logger

	group   *net.UDPAddr
	targets []target
}

// Config configures the SSDP server.
type Config struct {
	UDN        string
	DeviceType string
	Services   []string
	Location   string
	Logger     *slog.Logger
}

// New builds an SSDP server.
func New(cfg Config) (*Server, error) {
	group, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		udn:        cfg.UDN,
		deviceType: cfg.DeviceType,
		services:   cfg.Services,
		location:   cfg.Location,
		log:        cfg.Logger,
		group:      group,
	}
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
	conn, err := net.ListenMulticastUDP("udp4", nil, s.group)
	if err != nil {
		return fmt.Errorf("ssdp listen: %w (is another UPnP/DLNA server already using port 1900?)", err)
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(1 << 20)

	s.log.Info("ssdp listening", "group", multicastAddr, "location", s.location)

	// Initial announcement burst, then periodic re-announcements.
	s.announce(conn)
	ticker := time.NewTicker((maxAge / 3) * time.Second)
	defer ticker.Stop()

	go func() {
		<-ctx.Done()
		conn.Close() // unblock ReadFromUDP
	}()

	buf := make([]byte, 2048)
	go s.reannounceLoop(ctx, conn, ticker)

	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				s.byebye() // best-effort farewell on a fresh socket
				return nil
			default:
				s.log.Warn("ssdp read error", "err", err)
				return err
			}
		}
		s.handlePacket(conn, buf[:n], src)
	}
}

func (s *Server) reannounceLoop(ctx context.Context, conn *net.UDPConn, ticker *time.Ticker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.announce(conn)
		}
	}
}

// handlePacket parses a datagram and, if it is a matching M-SEARCH, replies.
func (s *Server) handlePacket(conn *net.UDPConn, pkt []byte, src *net.UDPAddr) {
	req := string(pkt)
	if !strings.HasPrefix(req, "M-SEARCH") {
		return
	}
	st := headerValue(req, "ST")
	if st == "" {
		return
	}

	// Honour the MX jitter window minimally to avoid reply storms.
	for _, t := range s.targets {
		if st == "ssdp:all" || st == t.nt {
			s.respond(conn, src, st, t)
		}
	}
}

// respond unicasts a search response for one target back to the requester.
func (s *Server) respond(conn *net.UDPConn, dst *net.UDPAddr, st string, t target) {
	// For a specific ST the response echoes that ST; for ssdp:all we use the
	// target's own NT as the ST.
	respST := st
	if st == "ssdp:all" {
		respST = t.nt
	}
	msg := "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=" + itoa(maxAge) + "\r\n" +
		"DATE: " + time.Now().UTC().Format(time.RFC1123) + "\r\n" +
		"EXT:\r\n" +
		"LOCATION: " + s.location + "\r\n" +
		"SERVER: " + serverString + "\r\n" +
		"ST: " + respST + "\r\n" +
		"USN: " + t.usn + "\r\n" +
		"BOOTID.UPNP.ORG: 1\r\n" +
		"CONFIGID.UPNP.ORG: 1\r\n\r\n"
	if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
		s.log.Debug("ssdp respond failed", "dst", dst.String(), "err", err)
	}
}

// announce multicasts ssdp:alive NOTIFY for every target.
func (s *Server) announce(conn *net.UDPConn) {
	for _, t := range s.targets {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + multicastAddr + "\r\n" +
			"CACHE-CONTROL: max-age=" + itoa(maxAge) + "\r\n" +
			"LOCATION: " + s.location + "\r\n" +
			"NT: " + t.nt + "\r\n" +
			"NTS: ssdp:alive\r\n" +
			"SERVER: " + serverString + "\r\n" +
			"USN: " + t.usn + "\r\n" +
			"BOOTID.UPNP.ORG: 1\r\n" +
			"CONFIGID.UPNP.ORG: 1\r\n\r\n"
		_, _ = conn.WriteToUDP([]byte(msg), s.group)
	}
}

// byebye multicasts ssdp:byebye for every target on a short-lived socket
// (called during shutdown after the main conn is closed).
func (s *Server) byebye() {
	conn, err := net.DialUDP("udp4", nil, s.group)
	if err != nil {
		return
	}
	defer conn.Close()
	for _, t := range s.targets {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + multicastAddr + "\r\n" +
			"NT: " + t.nt + "\r\n" +
			"NTS: ssdp:byebye\r\n" +
			"USN: " + t.usn + "\r\n\r\n"
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

func itoa(n int) string { return fmt.Sprintf("%d", n) }
