package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// SOAP request/response handling for UPnP control. UPnP uses SOAP 1.1 over
// HTTP POST; the action is named by the SOAPAction header
// ("urn:...:serviceType#ActionName").

// soapArg is one out-parameter in an action response.
type soapArg struct {
	Name  string
	Value string
}

// parseAction extracts the service type and action name from the SOAPAction
// header value, e.g. `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`.
func parseAction(header string) (serviceType, action string) {
	h := strings.Trim(strings.TrimSpace(header), `"`)
	if i := strings.LastIndex(h, "#"); i >= 0 {
		return h[:i], h[i+1:]
	}
	return "", h
}

// readBody reads and returns the full request body (SOAP envelopes are small).
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB is plenty
}

// unmarshalAction pulls the named action element's arguments out of a SOAP
// envelope into dst (a pointer to a struct whose fields are the args).
func unmarshalAction(body []byte, dst any) error {
	// The action element is nested in s:Envelope/s:Body. Walk to the first
	// element inside Body and unmarshal it into dst.
	//
	// Header must be skipped along with Envelope and Body. It is optional but
	// legal in SOAP 1.1 and some control points send it; decoding it as the
	// action produced empty arguments and a 402 Invalid Args for every such
	// request. Its children are skipped wholesale, not walked into.
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return fmt.Errorf("soap: no action element found")
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "Envelope", "Body":
			continue // descend
		case "Header":
			if err := dec.Skip(); err != nil {
				return err
			}
			continue
		}
		return dec.DecodeElement(dst, &se)
	}
}

const envelopeOpen = xml.Header +
	`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"` +
	` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
const envelopeClose = `</s:Body></s:Envelope>`

// soapBufs recycles response buffers. A Browse response is the largest thing
// this server builds, and it is built on every request.
var soapBufs = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledBuf caps what goes back in the pool, so one huge folder listing does
// not pin megabytes for the process lifetime.
const maxPooledBuf = 256 << 10

func getBuf() *bytes.Buffer {
	b := soapBufs.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuf(b *bytes.Buffer) {
	if b.Cap() <= maxPooledBuf {
		soapBufs.Put(b)
	}
}

// writeResponse writes a SOAP action response with the given out-arguments.
//
// Everything is assembled in one pooled buffer so the response can carry a
// Content-Length. Two reasons that matters:
//
//   - Without it Go switches to chunked transfer-encoding past 2 KB, and both
//     SCPD documents plus any non-trivial Browse result exceed that. Several
//     older Samsung/Sony/Panasonic DLNA stacks fail on chunked SOAP.
//   - The previous version escaped the DIDL into a string, appended that into a
//     second strings.Builder, then called String() again — each step copying and
//     each Builder doubling as it grew, peaking at several times the payload.
//     Escaping directly into the output buffer removes those copies.
func writeResponse(w http.ResponseWriter, serviceType, action string, args []soapArg) {
	b := getBuf()
	defer putBuf(b)

	size := len(envelopeOpen) + len(envelopeClose) + 64
	for _, a := range args {
		size += len(a.Value) + 2*len(a.Name) + 5
	}
	b.Grow(size + size/8) // headroom for escape expansion

	b.WriteString(envelopeOpen)
	fmt.Fprintf(b, `<u:%sResponse xmlns:u=%q>`, action, serviceType)
	for _, a := range args {
		b.WriteByte('<')
		b.WriteString(a.Name)
		b.WriteByte('>')
		writeEscaped(b, a.Value)
		b.WriteString("</")
		b.WriteString(a.Name)
		b.WriteByte('>')
	}
	fmt.Fprintf(b, `</u:%sResponse>`, action)
	b.WriteString(envelopeClose)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Ext", "")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	_, _ = w.Write(b.Bytes())
}

// writeFault writes a SOAP fault with a UPnP error code.
func writeFault(w http.ResponseWriter, code int, desc string) {
	b := getBuf()
	defer putBuf(b)

	b.WriteString(envelopeOpen)
	b.WriteString(`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`)
	b.WriteString(`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`)
	fmt.Fprintf(b, "<errorCode>%d</errorCode><errorDescription>", code)
	writeEscaped(b, desc)
	b.WriteString(`</errorDescription></UPnPError></detail></s:Fault>`)
	b.WriteString(envelopeClose)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(b.Bytes())
}

// escapeXML returns s escaped as XML character data. For hot paths prefer
// writeEscaped, which avoids materialising the result.
func escapeXML(s string) string {
	var b bytes.Buffer
	b.Grow(len(s))
	writeEscaped(&b, s)
	return b.String()
}

// writeEscaped writes s as XML character data, escaping the reserved characters
// in place.
//
// It copies unescaped runs wholesale rather than byte by byte, and takes a string
// so the caller does not have to allocate a []byte — which for a multi-megabyte
// DIDL document was itself a full copy. Only ASCII bytes are examined, so UTF-8
// sequences pass through untouched.
func writeEscaped(b *bytes.Buffer, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var repl string
		switch s[i] {
		case '&':
			repl = "&amp;"
		case '<':
			repl = "&lt;"
		case '>':
			repl = "&gt;"
		case '"':
			repl = "&#34;"
		case '\'':
			repl = "&#39;"
		case '\r':
			repl = "&#xD;"
		default:
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString(repl)
		last = i + 1
	}
	b.WriteString(s[last:])
}
