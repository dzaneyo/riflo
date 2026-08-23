package media

import (
	"bufio"
	"bytes"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/dzaneyo/riflo/internal/app"
)

const (
	hlsPlaylistTypeMaster   = "master"
	hlsPlaylistTypeMedia    = "media"
	hlsAvailabilityVOD      = "vod"
	hlsAvailabilityLive     = "live"
	hlsAvailabilityUnknown  = "unknown"
	hlsSegmentFormatTS      = "ts"
	hlsSegmentFormatFMP4    = "fmp4"
	hlsSegmentFormatMixed   = "mixed"
	hlsSegmentFormatUnknown = "unknown"
	maxHLSLineBytes         = 256 * 1024
)

var errNotHLSPlaylist = errors.New("response is not an HLS playlist")

// parsedHLSPlaylist keeps the real variant references private to the media
// operation that needs them. The public app.HLSInfo intentionally contains
// only sanitized display values.
type parsedHLSPlaylist struct {
	Info     app.HLSInfo
	Variants []hlsVariantReference
}

type hlsVariantReference struct {
	Index int
	URL   string
}

// parseHLSPlaylist extracts only bounded, non-sensitive playlist metadata.
// It deliberately does not retain segment, key, or variant URLs. The caller
// can use the returned summary for UI diagnostics without exposing signed URL
// query strings.
func parseHLSPlaylist(body []byte, base *url.URL) (app.HLSInfo, error) {
	parsed, err := parseHLSPlaylistDetailed(body, base)
	if err != nil {
		return app.HLSInfo{}, err
	}
	return parsed.Info, nil
}

func parseHLSPlaylistDetailed(body []byte, base *url.URL) (parsedHLSPlaylist, error) {
	if len(body) == 0 {
		return parsedHLSPlaylist{}, errNotHLSPlaylist
	}

	parsed := parsedHLSPlaylist{Info: app.HLSInfo{
		Availability:  hlsAvailabilityUnknown,
		SegmentFormat: hlsSegmentFormatUnknown,
	}}
	var (
		isM3U8          bool
		hasMediaMarkers bool
		hasEndList      bool
		playlistType    string
		pendingVariant  *app.HLSVariant
		pendingSegment  bool
		encryptionSeen  = make(map[string]struct{})
		variantIndex    int
	)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maxHLSLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "#EXTM3U" {
			isM3U8 = true
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			parsed.Info.PlaylistType = hlsPlaylistTypeMaster
			pendingVariant = parseVariant(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), variantIndex)
			variantIndex++
			continue
		}
		if pendingVariant != nil && !strings.HasPrefix(line, "#") {
			if reference := resolveHLSReference(base, line); reference != "" {
				pendingVariant.URLDisplay = displayHLSReference(base, line)
				parsed.Info.Variants = append(parsed.Info.Variants, *pendingVariant)
				parsed.Variants = append(parsed.Variants, hlsVariantReference{
					Index: pendingVariant.Index,
					URL:   reference,
				})
			}
			pendingVariant = nil
			continue
		}

		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
			pendingSegment = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
			if attrs := parseHLSAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:")); attrs["BYTERANGE"] != "" {
				parsed.Info.ByteRange = true
			}
			parsed.Info.SegmentFormat = mergeSegmentFormat(parsed.Info.SegmentFormat, hlsSegmentFormatFMP4)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
			parsed.Info.ByteRange = true
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
			method := strings.TrimSpace(parseHLSAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))["METHOD"])
			if method != "" && !strings.EqualFold(method, "NONE") {
				if _, exists := encryptionSeen[method]; !exists {
					encryptionSeen[method] = struct{}{}
					parsed.Info.EncryptionMethods = append(parsed.Info.EncryptionMethods, method)
				}
			}
		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
			hasMediaMarkers = true
			if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-PLAYLIST-TYPE:")), "VOD") {
				playlistType = hlsAvailabilityVOD
			}
		case line == "#EXT-X-ENDLIST":
			hasMediaMarkers = true
			hasEndList = true
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") ||
			strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") ||
			strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
		case pendingSegment && !strings.HasPrefix(line, "#"):
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
			hasMediaMarkers = true
			parsed.Info.SegmentCount++
			pendingSegment = false
			parsed.Info.SegmentFormat = mergeSegmentFormat(parsed.Info.SegmentFormat, segmentFormat(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedHLSPlaylist{}, err
	}
	if !isM3U8 {
		return parsedHLSPlaylist{}, errNotHLSPlaylist
	}
	if parsed.Info.PlaylistType == "" {
		if len(parsed.Info.Variants) > 0 {
			parsed.Info.PlaylistType = hlsPlaylistTypeMaster
		} else if hasMediaMarkers {
			parsed.Info.PlaylistType = hlsPlaylistTypeMedia
		} else {
			return parsedHLSPlaylist{}, errNotHLSPlaylist
		}
	}
	if parsed.Info.PlaylistType == hlsPlaylistTypeMaster {
		parsed.Info.Availability = hlsAvailabilityUnknown
	} else if hasEndList || playlistType == hlsAvailabilityVOD {
		parsed.Info.Availability = hlsAvailabilityVOD
	} else {
		parsed.Info.Availability = hlsAvailabilityLive
	}
	return parsed, nil
}

func parseVariant(raw string, index int) *app.HLSVariant {
	attrs := parseHLSAttributes(raw)
	variant := &app.HLSVariant{Index: index}
	variant.Bandwidth = parsePositiveInt64(attrs["BANDWIDTH"])
	variant.AverageBandwidth = parsePositiveInt64(attrs["AVERAGE-BANDWIDTH"])
	variant.Codecs = attrs["CODECS"]
	if width, height := parseResolution(attrs["RESOLUTION"]); width > 0 && height > 0 {
		variant.Width = width
		variant.Height = height
	}
	return variant
}

func parseHLSAttributes(raw string) map[string]string {
	values := make(map[string]string)
	start := 0
	inQuotes := false
	for index := 0; index <= len(raw); index++ {
		if index < len(raw) && raw[index] == '"' {
			inQuotes = !inQuotes
			continue
		}
		if index != len(raw) && (raw[index] != ',' || inQuotes) {
			continue
		}
		part := strings.TrimSpace(raw[start:index])
		if key, value, ok := strings.Cut(part, "="); ok {
			key = strings.ToUpper(strings.TrimSpace(key))
			value = strings.TrimSpace(value)
			value = strings.Trim(value, "\"")
			if key != "" {
				values[key] = value
			}
		}
		start = index + 1
	}
	return values
}

func parsePositiveInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func parseResolution(value string) (int, int) {
	width, height, ok := strings.Cut(strings.TrimSpace(value), "x")
	if !ok {
		return 0, 0
	}
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0
	}
	return w, h
}

func displayHLSReference(base *url.URL, raw string) string {
	resolved := resolveHLSReference(base, raw)
	if resolved == "" {
		return ""
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	return displayURL(parsed)
}

func resolveHLSReference(base *url.URL, raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return ""
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return parsed.String()
}

func segmentFormat(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return hlsSegmentFormatUnknown
	}
	path := strings.ToLower(parsed.Path)
	switch {
	case strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".aac"), strings.HasSuffix(path, ".mp3"):
		return hlsSegmentFormatTS
	case strings.HasSuffix(path, ".m4s"), strings.HasSuffix(path, ".mp4"), strings.HasSuffix(path, ".cmfv"), strings.HasSuffix(path, ".cmfa"):
		return hlsSegmentFormatFMP4
	default:
		return hlsSegmentFormatUnknown
	}
}

func mergeSegmentFormat(current, next string) string {
	if next == "" || next == hlsSegmentFormatUnknown {
		return current
	}
	if current == "" || current == hlsSegmentFormatUnknown {
		return next
	}
	if current == next || current == hlsSegmentFormatMixed {
		return current
	}
	return hlsSegmentFormatMixed
}
