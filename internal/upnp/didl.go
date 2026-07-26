package upnp

import (
	"encoding/xml"

	"beacon/internal/content"
)

// DIDL-Lite is the metadata format ContentDirectory returns inside a Browse
// response. Go's encoding/xml does not emit namespace prefixes nicely, so we
// use the well-worn trick of putting the prefix literally in the field tag
// (e.g. `dc:title`) and declaring the xmlns:* bindings as attributes on the
// root element.

type didlLite struct {
	XMLName    xml.Name        `xml:"DIDL-Lite"`
	XMLNS      string          `xml:"xmlns,attr"`
	XMLNSDC    string          `xml:"xmlns:dc,attr"`
	XMLNSUPNP  string          `xml:"xmlns:upnp,attr"`
	XMLNSDLNA  string          `xml:"xmlns:dlna,attr"`
	XMLNSSEC   string          `xml:"xmlns:sec,attr"`
	XMLNSPV    string          `xml:"xmlns:pv,attr"`
	Containers []didlContainer `xml:"container"`
	Items      []didlItem      `xml:"item"`
}

type didlContainer struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	Restricted int    `xml:"restricted,attr"`
	ChildCount int    `xml:"childCount,attr"`
	Title      string `xml:"dc:title"`
	Class      string `xml:"upnp:class"`
}

type didlItem struct {
	ID         string    `xml:"id,attr"`
	ParentID   string    `xml:"parentID,attr"`
	Restricted int       `xml:"restricted,attr"`
	Title      string    `xml:"dc:title"`
	Class      string    `xml:"upnp:class"`
	Date       string    `xml:"dc:date,omitempty"`
	AlbumArt   *didlAlbumArt `xml:"upnp:albumArtURI"`
	Res        []didlRes     `xml:"res"`
	// Subtitle metadata (three flavours for broad smart-TV compatibility).
	CaptionEx *didlCaption `xml:"sec:CaptionInfoEx"`
	Caption   *didlCaption `xml:"sec:CaptionInfo"`
	SubURI    string       `xml:"pv:subtitleFileUri,omitempty"`
	SubType   string       `xml:"pv:subtitleFileType,omitempty"`
}

type didlCaption struct {
	Type string `xml:"sec:type,attr"`
	URL  string `xml:",chardata"`
}

type didlAlbumArt struct {
	ProfileID string `xml:"dlna:profileID,attr,omitempty"`
	URL       string `xml:",chardata"`
}

type didlRes struct {
	ProtocolInfo string `xml:"protocolInfo,attr"`
	Size         int64  `xml:"size,attr,omitempty"`
	Duration     string `xml:"duration,attr,omitempty"`
	Resolution   string `xml:"resolution,attr,omitempty"`
	URL          string `xml:",chardata"`
}

// resURLFunc turns a media ID into an absolute streaming URL for the requesting
// client (host-dependent, so supplied per request).
type resURLFunc func(mediaID string) string

// subtitleMime maps a subtitle kind to its DLNA MIME type.
func subtitleMime(kind string) string {
	switch kind {
	case "ass":
		return "text/x-ssa"
	case "vtt":
		return "text/vtt"
	case "smi":
		return "application/smil"
	default:
		return "text/srt"
	}
}

// buildDIDL marshals a set of content objects into a DIDL-Lite XML document.
// resURL builds the absolute <res> URL for each item's media; subURL builds the
// URL for a sidecar subtitle; artURL builds the URL for a thumbnail/poster.
func buildDIDL(objs []content.Object, resURL, subURL, artURL resURLFunc) (string, error) {
	doc := didlLite{
		XMLNS:     "urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/",
		XMLNSDC:   "http://purl.org/dc/elements/1.1/",
		XMLNSUPNP: "urn:schemas-upnp-org:metadata-1-0/upnp/",
		XMLNSDLNA: "urn:schemas-dlna-org:metadata-1-0/",
		XMLNSSEC:  "http://www.sec.co.kr/",
		XMLNSPV:   "http://www.pv.com/pvns/",
	}
	for _, o := range objs {
		if o.IsContainer {
			doc.Containers = append(doc.Containers, didlContainer{
				ID:         o.ID,
				ParentID:   o.ParentID,
				Restricted: 1,
				ChildCount: o.ChildCount,
				Title:      o.Title,
				Class:      o.Class,
			})
			continue
		}
		item := didlItem{
			ID:         o.ID,
			ParentID:   o.ParentID,
			Restricted: 1,
			Title:      o.Title,
			Class:      o.Class,
			Date:       o.Date,
		}
		for _, r := range o.Resources {
			item.Res = append(item.Res, didlRes{
				ProtocolInfo: r.ProtocolInfo,
				Size:         r.Size,
				Duration:     r.Duration,
				Resolution:   r.Resolution,
				URL:          resURL(r.MediaID),
			})
		}
		if o.ArtworkID != "" {
			artwork := artURL(o.ArtworkID)
			item.AlbumArt = &didlAlbumArt{ProfileID: "JPEG_TN", URL: artwork}
			item.Res = append(item.Res, didlRes{
				ProtocolInfo: "http-get:*:image/jpeg:DLNA.ORG_PN=JPEG_TN",
				URL:          artwork,
			})
		}
		if o.SubtitleID != "" {
			subMime := subtitleMime(o.SubtitleKind)
			url := subURL(o.SubtitleID)
			// A subtitle <res> plus Samsung sec: and pv: hints — different TVs
			// look for different ones.
			item.Res = append(item.Res, didlRes{
				ProtocolInfo: "http-get:*:" + subMime + ":*",
				URL:          url,
			})
			item.CaptionEx = &didlCaption{Type: o.SubtitleKind, URL: url}
			item.Caption = &didlCaption{Type: o.SubtitleKind, URL: url}
			item.SubURI = url
			item.SubType = o.SubtitleKind
		}
		doc.Items = append(doc.Items, item)
	}

	out, err := xml.Marshal(doc)
	if err != nil {
		return "", err
	}
	// No XML declaration here: this document is embedded (escaped) inside the
	// SOAP <Result>, and a leading <?xml?> there trips up some TV parsers.
	return string(out), nil
}
