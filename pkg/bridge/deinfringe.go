package bridge

import (
	"regexp"
	"strings"
)

// deInfringe rewrites filenames that Real-Debrid's 451 filter rejects.
// Mirrors DMM's debrid uploader (debrid/src/naming.ts) and DMM's utils/deInfringe.ts:
//   WEB-DL -> WEB.DL
//   bluray.x264 / hdtv.x264 / web.x264 -> bluray-x264 (dot to dash)
//   BDRip / HDRip / WEBRip / DVDrip -> BD-Rip (insert dash)
// Newer codecs (x265/HEVC/AV1) and service tags are never blocked.
var (
	blurayX264Re = regexp.MustCompile(`(?i)(bluray|hdtv|web)\.(x264|xvid|h264)`)
	webDLRe      = regexp.MustCompile(`(?i)web-dl`)
	ripRe        = regexp.MustCompile(`(?i)(web|bd|hd|dvd)rip`)
)

// DeInfringe applies RD's infringing-file filename transforms.
func DeInfringe(name string) string {
	// bluray.x264 -> bluray-x264
	name = blurayX264Re.ReplaceAllStringFunc(name, func(m string) string {
		parts := strings.Split(m, ".")
		if len(parts) == 2 {
			return parts[0] + "-" + parts[1]
		}
		return m
	})
	// WEB-DL -> WEB.DL (preserve case of matched)
	name = webDLRe.ReplaceAllStringFunc(name, func(m string) string {
		// m is like "WEB-DL" or "web-dl" – replace the dash with dot
		return strings.Replace(m, "-", ".", 1)
	})
	// BDRip -> BD-Rip (case-preserving)
	name = ripRe.ReplaceAllStringFunc(name, func(m string) string {
		lower := strings.ToLower(m)
		var prefix string
		switch {
		case strings.HasPrefix(lower, "web"):
			prefix = m[:3]
			return prefix + "-" + m[3:]
		case strings.HasPrefix(lower, "bd"):
			prefix = m[:2]
			return prefix + "-" + m[2:]
		case strings.HasPrefix(lower, "hd"):
			prefix = m[:2]
			return prefix + "-" + m[2:]
		case strings.HasPrefix(lower, "dvd"):
			prefix = m[:3]
			return prefix + "-" + m[3:]
		}
		return m
	})
	return name
}

// NeedsDeInfringe reports whether DeInfringe would change the name.
func NeedsDeInfringe(name string) bool {
	return DeInfringe(name) != name
}
