package upnp

import (
	"bytes"
	"text/template"
)

// DeviceInfo is the identity presented in the device description document.
type DeviceInfo struct {
	FriendlyName string
	UDN          string // "uuid:...."
	Manufacturer string
	ModelName    string
	ModelNumber  string
}

// deviceDescTmpl renders the UPnP device description (rootDesc.xml). URLBase is
// intentionally omitted so clients resolve relative URLs against the request
// host — this keeps the same document valid across every network interface.
var deviceDescTmpl = template.Must(template.New("desc").Parse(
	`<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>
    <deviceType>{{.DeviceType}}</deviceType>
    <friendlyName>{{.Info.FriendlyName}}</friendlyName>
    <manufacturer>{{.Info.Manufacturer}}</manufacturer>
    <modelName>{{.Info.ModelName}}</modelName>
    <modelNumber>{{.Info.ModelNumber}}</modelNumber>
    <UDN>{{.Info.UDN}}</UDN>
    <serviceList>
      <service>
        <serviceType>{{.CDType}}</serviceType>
        <serviceId>{{.CDID}}</serviceId>
        <SCPDURL>{{.CDSCPD}}</SCPDURL>
        <controlURL>{{.CDCtl}}</controlURL>
        <eventSubURL>{{.CDEvt}}</eventSubURL>
      </service>
      <service>
        <serviceType>{{.CMType}}</serviceType>
        <serviceId>{{.CMID}}</serviceId>
        <SCPDURL>{{.CMSCPD}}</SCPDURL>
        <controlURL>{{.CMCtl}}</controlURL>
        <eventSubURL>{{.CMEvt}}</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>
`))

// deviceDescription renders the device description XML for the given identity.
func deviceDescription(info DeviceInfo) ([]byte, error) {
	// text/template does not escape XML, so escape the one user-supplied field.
	info.FriendlyName = escapeXML(info.FriendlyName)
	data := struct {
		Info       DeviceInfo
		DeviceType string
		CDType, CDID, CDSCPD, CDCtl, CDEvt string
		CMType, CMID, CMSCPD, CMCtl, CMEvt string
	}{
		Info:       info,
		DeviceType: DeviceType,
		CDType:     ServiceContentDirectory,
		CDID:       serviceContentDirectoryID,
		CDSCPD:     PathSCPDContentDir,
		CDCtl:      PathCtlContentDir,
		CDEvt:      PathEvtContentDir,
		CMType:     ServiceConnectionManager,
		CMID:       serviceConnectionMgrID,
		CMSCPD:     PathSCPDConnMgr,
		CMCtl:      PathCtlConnMgr,
		CMEvt:      PathEvtConnMgr,
	}
	var buf bytes.Buffer
	if err := deviceDescTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
