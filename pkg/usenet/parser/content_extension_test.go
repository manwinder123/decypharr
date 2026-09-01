package parser

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Ported from upstream e7ca363 (sirrobot01 beta) reliability tests — the
// cases that only depend on detectFileTypeAndExtensionFromContent.

func TestContentDetectionInfersExtensionForObfuscatedMedia(t *testing.T) {
	p := &NZBParser{}
	cases := []struct {
		name      string
		data      []byte
		fileType  storage.NZBFileType
		extension string
	}{
		{"matroska", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0x02, 0x03, 0x04}, storage.NZBFileTypeMedia, ".mkv"},
		{"mp4", []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, storage.NZBFileTypeMedia, ".mp4"},
		{"avi", []byte{'R', 'I', 'F', 'F', 0x00, 0x00, 0x00, 0x00, 'A', 'V', 'I', ' ', 0x00}, storage.NZBFileTypeMedia, ".avi"},
		{"parity", []byte{'P', 'A', 'R', 2, 0, 'P', 'K', 'T'}, storage.NZBFileTypeIgnore, ""},
		{"rar4", []byte{'R', 'a', 'r', '!', 0x1A, 0x07, 0x00}, storage.NZBFileTypeRar, ""},
		{"mpeg ts", func() []byte { b := make([]byte, 190); b[0] = 0x47; b[188] = 0x47; return b }(), storage.NZBFileTypeMedia, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fileType, extension := p.detectFileTypeAndExtensionFromContent(tt.data)
			if fileType != tt.fileType || extension != tt.extension {
				t.Fatalf("content classification = (%v, %q), want (%v, %q)", fileType, extension, tt.fileType, tt.extension)
			}
		})
	}
}

func TestContentDetectionDoesNotPanicOnShortBuffers(t *testing.T) {
	p := &NZBParser{}

	for n := 0; n < 24; n++ {
		data := make([]byte, n)
		if n > 0 {
			data[0] = 0x47
		}
		// Must not panic for any short buffer; exact-boundary TS behavior is
		// covered in parser_test.go.
		_, _ = p.detectFileTypeAndExtensionFromContent(data)
	}
}
