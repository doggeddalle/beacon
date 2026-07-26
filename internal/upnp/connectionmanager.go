package upnp

import (
	"net/http"
	"strings"

	"beacon/internal/content"
)

// ConnectionManager implements the minimal UPnP ConnectionManager:1 service. As
// a pure media source with no transcoding, our GetProtocolInfo advertises the
// formats we serve, and there are never any active managed connections.
type ConnectionManager struct{}

// NewConnectionManager creates the service.
func NewConnectionManager() *ConnectionManager { return &ConnectionManager{} }

func (cm *ConnectionManager) handleControl(w http.ResponseWriter, r *http.Request) {
	_, action := parseAction(r.Header.Get("SOAPAction"))
	switch action {
	case "GetProtocolInfo":
		writeResponse(w, ServiceConnectionManager, action, []soapArg{
			{"Source", sourceProtocolInfo()},
			{"Sink", ""},
		})
	case "GetCurrentConnectionIDs":
		writeResponse(w, ServiceConnectionManager, action, []soapArg{{"ConnectionIDs", "0"}})
	case "GetCurrentConnectionInfo":
		writeResponse(w, ServiceConnectionManager, action, []soapArg{
			{"RcsID", "-1"},
			{"AVTransportID", "-1"},
			{"ProtocolInfo", ""},
			{"PeerConnectionManager", ""},
			{"PeerConnectionID", "-1"},
			{"Direction", "Output"},
			{"Status", "OK"},
		})
	default:
		writeFault(w, 401, "Invalid Action")
	}
}

// sourceProtocolInfo lists the http-get protocols we can serve, one per known
// media MIME type. A trailing "*" wildcard entry keeps permissive clients happy.
func sourceProtocolInfo() string {
	mimes := content.KnownMimeTypes()
	parts := make([]string, 0, len(mimes)+1)
	for _, m := range mimes {
		parts = append(parts, "http-get:*:"+m+":*")
	}
	parts = append(parts, "http-get:*:*:*")
	return strings.Join(parts, ",")
}
