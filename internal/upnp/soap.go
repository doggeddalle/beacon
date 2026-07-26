package upnp

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	// The action element is nested in s:Envelope/s:Body. Decode by walking to
	// the first element inside Body and unmarshalling it into dst.
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	depth := 0
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
		depth++
		local := se.Name.Local
		if local == "Envelope" || local == "Body" {
			continue
		}
		// First element inside Body is the action; decode it into dst.
		return dec.DecodeElement(dst, &se)
	}
}

// writeResponse writes a SOAP action response with the given out-arguments.
func writeResponse(w http.ResponseWriter, serviceType, action string, args []soapArg) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%sResponse xmlns:u=%q>`, action, serviceType)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>%s</%s>", a.Name, escapeXML(a.Value), a.Name)
	}
	fmt.Fprintf(&b, `</u:%sResponse>`, action)
	b.WriteString(`</s:Body></s:Envelope>`)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Ext", "")
	_, _ = io.WriteString(w, b.String())
}

// writeFault writes a SOAP fault with a UPnP error code.
func writeFault(w http.ResponseWriter, code int, desc string) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`)
	b.WriteString(`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`)
	fmt.Fprintf(&b, "<errorCode>%d</errorCode><errorDescription>%s</errorDescription>", code, escapeXML(desc))
	b.WriteString(`</UPnPError></detail></s:Fault>`)
	b.WriteString(`</s:Body></s:Envelope>`)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, b.String())
}

// escapeXML escapes a string for safe inclusion as XML character data.
func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
