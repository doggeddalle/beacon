package upnp

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"

	"beacon/internal/content"
)

// ContentDirectory implements the UPnP ContentDirectory:1 service, backed by a
// content.Backend.
type ContentDirectory struct {
	backend content.Backend
	log     *slog.Logger
	// systemUpdateID increments whenever the library changes; clients that
	// track it know to re-browse. Bumped by the auto-update engine (Phase 3).
	systemUpdateID atomic.Uint32
}

// NewContentDirectory creates the service over the given backend.
func NewContentDirectory(backend content.Backend, log *slog.Logger) *ContentDirectory {
	cd := &ContentDirectory{backend: backend, log: log}
	cd.systemUpdateID.Store(1)
	return cd
}

// BumpUpdateID increments the ContentDirectory SystemUpdateID, signalling to
// clients that the library has changed. Safe for concurrent use.
func (cd *ContentDirectory) BumpUpdateID() uint32 {
	return cd.systemUpdateID.Add(1)
}

type browseArgs struct {
	ObjectID       string `xml:"ObjectID"`
	BrowseFlag     string `xml:"BrowseFlag"`
	Filter         string `xml:"Filter"`
	StartingIndex  int    `xml:"StartingIndex"`
	RequestedCount int    `xml:"RequestedCount"`
	SortCriteria   string `xml:"SortCriteria"`
}

// handleControl dispatches a SOAP control request for ContentDirectory.
func (cd *ContentDirectory) handleControl(w http.ResponseWriter, r *http.Request, resURL, subURL, artURL resURLFunc) {
	body, err := readBody(r)
	if err != nil {
		writeFault(w, 402, "Invalid Args")
		return
	}
	_, action := parseAction(r.Header.Get("SOAPAction"))
	switch action {
	case "Browse":
		cd.browse(w, body, resURL, subURL, artURL)
	case "GetSystemUpdateID":
		writeResponse(w, ServiceContentDirectory, action, []soapArg{
			{"Id", strconv.FormatUint(uint64(cd.systemUpdateID.Load()), 10)},
		})
	case "GetSortCapabilities":
		writeResponse(w, ServiceContentDirectory, action, []soapArg{{"SortCaps", "dc:title"}})
	case "GetSearchCapabilities":
		writeResponse(w, ServiceContentDirectory, action, []soapArg{{"SearchCaps", ""}})
	default:
		writeFault(w, 401, "Invalid Action")
	}
}

func (cd *ContentDirectory) browse(w http.ResponseWriter, body []byte, resURL, subURL, artURL resURLFunc) {
	var args browseArgs
	if err := unmarshalAction(body, &args); err != nil {
		writeFault(w, 402, "Invalid Args")
		return
	}

	updateID := strconv.FormatUint(uint64(cd.systemUpdateID.Load()), 10)

	switch args.BrowseFlag {
	case "BrowseMetadata":
		obj, err := cd.backend.Object(args.ObjectID)
		if err != nil {
			cd.log.Warn("browse metadata failed", "object", args.ObjectID, "err", err)
			writeFault(w, 701, "No such object")
			return
		}
		didl, err := buildDIDL([]content.Object{obj}, resURL, subURL, artURL)
		if err != nil {
			writeFault(w, 501, "Action Failed")
			return
		}
		cd.writeBrowse(w, didl, 1, 1, updateID)

	case "BrowseDirectChildren":
		children, err := cd.backend.Children(args.ObjectID)
		if err != nil {
			cd.log.Warn("browse children failed", "object", args.ObjectID, "err", err)
			writeFault(w, 701, "No such object")
			return
		}
		total := len(children)
		page := paginate(children, args.StartingIndex, args.RequestedCount)
		didl, err := buildDIDL(page, resURL, subURL, artURL)
		if err != nil {
			writeFault(w, 501, "Action Failed")
			return
		}
		cd.writeBrowse(w, didl, len(page), total, updateID)

	default:
		writeFault(w, 402, "Invalid Args")
	}
}

func (cd *ContentDirectory) writeBrowse(w http.ResponseWriter, didl string, returned, total int, updateID string) {
	writeResponse(w, ServiceContentDirectory, "Browse", []soapArg{
		{"Result", didl},
		{"NumberReturned", strconv.Itoa(returned)},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", updateID},
	})
}

// paginate applies UPnP StartingIndex/RequestedCount semantics. A RequestedCount
// of 0 means "all remaining from StartingIndex".
func paginate(objs []content.Object, start, count int) []content.Object {
	if start < 0 {
		start = 0
	}
	if start >= len(objs) {
		return nil
	}
	end := len(objs)
	if count > 0 && start+count < end {
		end = start + count
	}
	return objs[start:end]
}
