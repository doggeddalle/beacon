package upnp

import _ "embed"

// UPnP device and service identifiers for a UPnP-AV MediaServer:1.
const (
	DeviceType = "urn:schemas-upnp-org:device:MediaServer:1"

	ServiceContentDirectory   = "urn:schemas-upnp-org:service:ContentDirectory:1"
	ServiceConnectionManager  = "urn:schemas-upnp-org:service:ConnectionManager:1"
	serviceContentDirectoryID = "urn:upnp-org:serviceId:ContentDirectory"
	serviceConnectionMgrID    = "urn:upnp-org:serviceId:ConnectionManager"
)

// Well-known URL paths on the HTTP server.
const (
	PathDeviceDesc = "/rootDesc.xml"

	PathSCPDContentDir = "/scpd/ContentDirectory.xml"
	PathCtlContentDir  = "/ctl/ContentDirectory"
	PathEvtContentDir  = "/evt/ContentDirectory"
	PathSCPDConnMgr    = "/scpd/ConnectionManager.xml"
	PathCtlConnMgr     = "/ctl/ConnectionManager"
	PathEvtConnMgr     = "/evt/ConnectionManager"
	PathMediaPrefix    = "/media/"    // + object ID
	PathSubtitlePrefix = "/subtitle/" // + object ID
	PathThumbPrefix    = "/thumb/"    // + object ID
)

//go:embed xml/ContentDirectory.xml
var scpdContentDirectory []byte

//go:embed xml/ConnectionManager.xml
var scpdConnectionManager []byte
