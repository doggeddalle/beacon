package ssdp

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{
		UDN:          "uuid:test-1234",
		DeviceType:   "urn:schemas-upnp-org:device:MediaServer:1",
		Services:     []string{"urn:schemas-upnp-org:service:ContentDirectory:1"},
		LocationPath: "/rootDesc.xml",
		HTTPPort:     8322,
		Logger:       discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The advertised set must be the conventional root-device / UDN / device-type /
// per-service tuple, or some control points never match the server.
func TestBuildTargets(t *testing.T) {
	s := newTestServer(t)
	want := map[string]string{
		"upnp:rootdevice": "uuid:test-1234::upnp:rootdevice",
		"uuid:test-1234":  "uuid:test-1234",
		"urn:schemas-upnp-org:device:MediaServer:1":       "uuid:test-1234::urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:service:ContentDirectory:1": "uuid:test-1234::urn:schemas-upnp-org:service:ContentDirectory:1",
	}
	if len(s.targets) != len(want) {
		t.Fatalf("got %d targets, want %d: %+v", len(s.targets), len(want), s.targets)
	}
	for _, tg := range s.targets {
		usn, ok := want[tg.nt]
		if !ok {
			t.Errorf("unexpected target NT %q", tg.nt)
			continue
		}
		if tg.usn != usn {
			t.Errorf("NT %q has USN %q, want %q", tg.nt, tg.usn, usn)
		}
	}
}

func TestHeaderValue(t *testing.T) {
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"mx: 3\r\n" +
		"ST: upnp:rootdevice\r\n\r\n"

	cases := map[string]string{
		"HOST": "239.255.255.250:1900",
		"MAN":  "ssdp:discover", // surrounding quotes stripped
		"MX":   "3",             // header names are case-insensitive
		"ST":   "upnp:rootdevice",
		"NOPE": "",
	}
	for name, want := range cases {
		if got := headerValue(msg, name); got != want {
			t.Errorf("headerValue(%q) = %q, want %q", name, got, want)
		}
	}
}

// UPnP requires a random delay within the MX window so a roomful of devices does
// not answer a broadcast in unison. The old code documented this and did not do
// it, replying immediately and firing all targets back to back.
func TestJitterRespectsMXWindow(t *testing.T) {
	if d := jitter(""); d != 0 {
		t.Errorf("missing MX should mean no delay, got %v", d)
	}
	if d := jitter("0"); d != 0 {
		t.Errorf("MX=0 should mean no delay, got %v", d)
	}
	if d := jitter("garbage"); d != 0 {
		t.Errorf("unparseable MX should mean no delay, got %v", d)
	}
	for range 50 {
		if d := jitter("3"); d < 0 || d >= 3*time.Second {
			t.Fatalf("jitter(3) = %v, want [0s, 3s)", d)
		}
	}
	// The spec caps MX at 5; a larger claim must not extend the window.
	for range 50 {
		if d := jitter("120"); d < 0 || d >= 5*time.Second {
			t.Fatalf("jitter(120) = %v, want it clamped below 5s", d)
		}
	}
}

// BOOTID must change between runs, or a control point that cached a LOCATION
// against an old address never re-fetches the device description.
func TestBootIDIsSetAndAdvances(t *testing.T) {
	s := newTestServer(t)
	if got := s.bootID.Load(); got <= 0 {
		t.Fatalf("bootID = %d, want a positive value", got)
	}
	if got := s.bootID.Load(); got > 0x7fffffff {
		t.Errorf("bootID = %d exceeds the spec's 31-bit range", got)
	}
	before := s.bootID.Load()
	if after := s.bootID.Add(1); after != before+1 {
		t.Errorf("bootID did not advance: %d -> %d", before, after)
	}
}

func TestURLForUsesGivenAddress(t *testing.T) {
	s := newTestServer(t)
	got := s.urlFor(net.ParseIP("192.168.1.50"))
	if got != "http://192.168.1.50:8322/rootDesc.xml" {
		t.Errorf("urlFor = %q", got)
	}
}

// The search response must be a well-formed HTTP-over-UDP message with the
// headers control points key on.
func TestRespondMessageShape(t *testing.T) {
	s := newTestServer(t)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a UDP socket here: %v", err)
	}
	defer pc.Close()

	recv, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a UDP socket here: %v", err)
	}
	defer recv.Close()

	// Send straight over the raw socket, mirroring what respond writes.
	dst := recv.LocalAddr().(*net.UDPAddr)
	target := s.targets[0]
	msg := s.searchResponse("upnp:rootdevice", target, "http://192.168.1.50:8322/rootDesc.xml")
	if _, err := pc.WriteTo([]byte(msg), dst); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 2048)
	recv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := recv.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := string(buf[:n])

	for _, want := range []string{
		"HTTP/1.1 200 OK",
		"CACHE-CONTROL: max-age=1800",
		"LOCATION: http://192.168.1.50:8322/rootDesc.xml",
		"ST: upnp:rootdevice",
		"USN: uuid:test-1234::upnp:rootdevice",
		"BOOTID.UPNP.ORG:",
		"EXT:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response missing %q:\n%s", want, got)
		}
	}
	// DATE must be an HTTP-date, which ends in GMT. time.RFC1123 renders "UTC"
	// and is not valid here.
	if strings.Contains(got, "UTC") {
		t.Errorf("DATE header is not an HTTP-date (renders UTC, want GMT):\n%s", got)
	}
	if !strings.Contains(got, "GMT") {
		t.Errorf("DATE header missing or not GMT:\n%s", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Error("response must end with a blank line")
	}
}
