package content

import (
	"path/filepath"
	"sort"
	"strings"
)

// mediaKind classifies a file for UPnP purposes.
type mediaKind int

const (
	kindUnknown mediaKind = iota
	kindVideo
	kindAudio
	kindImage
)

// mediaInfo is the static type information derived from a file extension.
type mediaInfo struct {
	kind  mediaKind
	class string // UPnP object class
	mime  string
}

// extTable maps lower-case file extensions (with dot) to their media info.
var extTable = map[string]mediaInfo{
	// Video
	".mp4":  {kindVideo, "object.item.videoItem", "video/mp4"},
	".m4v":  {kindVideo, "object.item.videoItem", "video/mp4"},
	".mkv":  {kindVideo, "object.item.videoItem", "video/x-matroska"},
	".avi":  {kindVideo, "object.item.videoItem", "video/x-msvideo"},
	".mov":  {kindVideo, "object.item.videoItem", "video/quicktime"},
	".wmv":  {kindVideo, "object.item.videoItem", "video/x-ms-wmv"},
	".mpg":  {kindVideo, "object.item.videoItem", "video/mpeg"},
	".mpeg": {kindVideo, "object.item.videoItem", "video/mpeg"},
	".ts":   {kindVideo, "object.item.videoItem", "video/mp2t"},
	".m2ts": {kindVideo, "object.item.videoItem", "video/mp2t"},
	".mts":  {kindVideo, "object.item.videoItem", "video/mp2t"},
	".flv":  {kindVideo, "object.item.videoItem", "video/x-flv"},
	".webm": {kindVideo, "object.item.videoItem", "video/webm"},
	".vob":  {kindVideo, "object.item.videoItem", "video/mpeg"},
	".3gp":  {kindVideo, "object.item.videoItem", "video/3gpp"},
	".divx": {kindVideo, "object.item.videoItem", "video/x-msvideo"},

	// Audio
	".mp3":  {kindAudio, "object.item.audioItem.musicTrack", "audio/mpeg"},
	".flac": {kindAudio, "object.item.audioItem.musicTrack", "audio/flac"},
	".aac":  {kindAudio, "object.item.audioItem.musicTrack", "audio/aac"},
	".m4a":  {kindAudio, "object.item.audioItem.musicTrack", "audio/mp4"},
	".ogg":  {kindAudio, "object.item.audioItem.musicTrack", "audio/ogg"},
	".oga":  {kindAudio, "object.item.audioItem.musicTrack", "audio/ogg"},
	".opus": {kindAudio, "object.item.audioItem.musicTrack", "audio/opus"},
	".wav":  {kindAudio, "object.item.audioItem.musicTrack", "audio/wav"},
	".wma":  {kindAudio, "object.item.audioItem.musicTrack", "audio/x-ms-wma"},

	// Images
	".jpg":  {kindImage, "object.item.imageItem.photo", "image/jpeg"},
	".jpeg": {kindImage, "object.item.imageItem.photo", "image/jpeg"},
	".png":  {kindImage, "object.item.imageItem.photo", "image/png"},
	".gif":  {kindImage, "object.item.imageItem.photo", "image/gif"},
	".bmp":  {kindImage, "object.item.imageItem.photo", "image/bmp"},
	".webp": {kindImage, "object.item.imageItem.photo", "image/webp"},
}

// KnownMimeTypes returns the sorted, de-duplicated set of MIME types Beacon can
// serve. Used to advertise ConnectionManager source protocols.
func KnownMimeTypes() []string {
	seen := map[string]bool{}
	var out []string
	for _, info := range extTable {
		if !seen[info.mime] {
			seen[info.mime] = true
			out = append(out, info.mime)
		}
	}
	sort.Strings(out)
	return out
}

// lookupMedia returns the media info for a filename, and whether it is a
// recognised media file.
func lookupMedia(name string) (mediaInfo, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	info, ok := extTable[ext]
	return info, ok
}

// DLNAFlags is the contentFeatures value for streamed media (audio and video):
// byte-range seeking supported (OP=01), not transcoded (CI=0), and the
// TM_S|TM_B|HTTP_STALLING|DLNA_V15 flag word.
const DLNAFlags = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;" +
	"DLNA.ORG_FLAGS=01700000000000000000000000000000"

// imageDLNAFlags is the contentFeatures value for still images: TM_I|TM_B.
//
// Images must advertise TM_I (interactive, 0x00800000), not TM_S. The single
// streaming constant above used to be applied to every kind, so a renderer that
// honours the transfer-mode bits would refuse to fetch photos as interactive
// content.
const imageDLNAFlags = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;" +
	"DLNA.ORG_FLAGS=00D00000000000000000000000000000"

// ContentFeatures returns the contentFeatures.dlna.org value for a MIME type.
func ContentFeatures(mime string) string {
	if isImageMime(mime) {
		return imageDLNAFlags
	}
	return DLNAFlags
}

// TransferModeFor returns the DLNA transfer mode a MIME type should default to.
func TransferModeFor(mime string) string {
	if isImageMime(mime) {
		return "Interactive"
	}
	return "Streaming"
}

func isImageMime(mime string) bool { return strings.HasPrefix(mime, "image/") }

// dlnaProtocolInfo builds the `res@protocolInfo` string for a MIME type.
func dlnaProtocolInfo(mime string) string {
	return "http-get:*:" + mime + ":" + ContentFeatures(mime)
}

// MimeType returns the MIME type for a media filename and whether it is a
// recognised media file.
func MimeType(name string) (string, bool) {
	info, ok := lookupMedia(name)
	return info.mime, ok
}

// Classify returns the UPnP object class and MIME type for a media filename,
// and whether it is a recognised media file.
func Classify(name string) (class, mime string, ok bool) {
	info, ok := lookupMedia(name)
	return info.class, info.mime, ok
}

// ProtocolInfo builds the DLNA `res@protocolInfo` string for a MIME type.
func ProtocolInfo(mime string) string { return dlnaProtocolInfo(mime) }
