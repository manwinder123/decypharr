package bridge

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"sort"
	"time"

	"github.com/jackpal/bencode-go"
)

// TorrentFile describes a file to be included in the webseed torrent.
type TorrentFile struct {
	// Path is the display name / relative path (e.g. "Movie 2024/Movie.mkv").
	// Slashed paths are split into path components for the torrent file tree.
	Path string
	// Size is the byte length.
	Size int64
	// WebseedURL is the HTTP URL RD will fetch this file from (must be public).
	WebseedURL string
}

// BuildWebseedTorrent generates a BitTorrent v1 .torrent file (bencoded dict)
// with webseed URLs. The resulting infohash can be added to Real-Debrid via
// PUT /torrents/addTorrent with the file bytes. RD will fetch each file via
// the webseed URLs, bypassing its infringing filename filter when DeInfringe
// has been applied to Path.
//
// Piece length is fixed at 4 MiB (common for large media). Pieces field is
// zeroed (webseed-only torrents don't need valid hashes; RD ignores them for
// webseed pulls, but we fill with zeros to keep the file valid).
func BuildWebseedTorrent(files []TorrentFile, name string) ([]byte, string, error) {
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no files")
	}
	if name == "" {
		name = files[0].Path
	}

	const pieceLength = 4 * 1024 * 1024

	// Sort for determinism (infohash stability)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// Apply de-infringement to filenames so the torrent as stored on RD passes
	// RD's filename filter. Preserve original size/webseed but rewrite path.
	for i := range files {
		// Only rewrite the last path component (filename), not directory names
		files[i].Path = DeInfringe(files[i].Path)
	}

	// Build info dict
	type fileDict struct {
		Length int64    `bencode:"length"`
		Path   []string `bencode:"path"`
	}
	type infoDict struct {
		Name        string     `bencode:"name"`
		PieceLength int64      `bencode:"piece length"`
		Pieces      string     `bencode:"pieces"`
		Files       []fileDict `bencode:"files,omitempty"`
		Length      int64      `bencode:"length,omitempty"`
	}

	var info infoDict
	info.Name = name
	info.PieceLength = pieceLength

	// Total size → piece count → zeroed pieces
	var total int64
	for _, f := range files {
		total += f.Size
	}
	pieceCount := (total + pieceLength - 1) / pieceLength
	if pieceCount == 0 {
		pieceCount = 1
	}
	info.Pieces = string(bytes.Repeat([]byte{0}, int(pieceCount*20)))

	if len(files) == 1 && !containsSlash(files[0].Path) {
		// Single-file torrent: top-level is the file itself
		info.Length = files[0].Size
	} else {
		// Multi-file: info.files list
		for _, f := range files {
			parts := splitPath(f.Path)
			info.Files = append(info.Files, fileDict{Length: f.Size, Path: parts})
		}
	}

	// Collect unique webseed URLs (one per file, in sorted order)
	webseeds := make([]string, 0, len(files))
	seen := make(map[string]struct{})
	for _, f := range files {
		if f.WebseedURL != "" {
			if _, ok := seen[f.WebseedURL]; !ok {
				seen[f.WebseedURL] = struct{}{}
				webseeds = append(webseeds, f.WebseedURL)
			}
		}
	}
	sort.Strings(webseeds)

	// Outer torrent dict
	torrentDict := map[string]interface{}{
		"announce":      "http://tracker.example.com/announce",
		"creation date": time.Now().Unix(),
		"created by":    "decypharr-bridge",
		"encoding":      "UTF-8",
		"info":          info,
	}
	if len(webseeds) > 0 {
		// BEP 19 webseed: url-list can be string or list
		if len(webseeds) == 1 {
			torrentDict["url-list"] = webseeds[0]
		} else {
			torrentDict["url-list"] = webseeds
		}
	}

	var buf bytes.Buffer
	if err := bencode.Marshal(&buf, torrentDict); err != nil {
		return nil, "", fmt.Errorf("bencode: %w", err)
	}

	// Infohash = SHA1(bencoded info dict)
	var infoBuf bytes.Buffer
	if err := bencode.Marshal(&infoBuf, info); err != nil {
		return nil, "", fmt.Errorf("bencode info: %w", err)
	}
	h := sha1.Sum(infoBuf.Bytes())
	infohash := fmt.Sprintf("%x", h)

	return buf.Bytes(), infohash, nil
}

func containsSlash(s string) bool {
	return bytes.Contains([]byte(s), []byte("/"))
}

func splitPath(p string) []string {
	parts := bytes.Split([]byte(p), []byte("/"))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	if len(out) == 0 {
		return []string{p}
	}
	return out
}
