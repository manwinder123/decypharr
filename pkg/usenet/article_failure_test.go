package usenet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestRecordArticleFailurePersistsExactIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "article_failures.jsonl")
	u := &Usenet{
		logger:         zerolog.Nop(),
		failureLogPath: path,
	}
	segment := storage.NZBSegment{
		Number:           7,
		MessageID:        "<article@example>",
		Group:            "alt.test",
		Bytes:            768000,
		SegmentDataStart: 42,
	}
	metadata := &nntp.YencMetadata{
		Name:     "part07.mkv",
		Size:     768000,
		Part:     7,
		Total:    310,
		Begin:    4608001,
		End:      5376000,
		Offset:   4608000,
		PartSize: 768000,
	}
	err := &nntp.Error{
		Type:    nntp.ErrorTypeYencDecode,
		Message: "yEnc decode failed",
		Err:     errors.New("expected decoded data to have CRC32 hash 0x12345678 but got 0x87654321: crc32 mismatch"),
	}

	u.recordArticleFailure("nzb-123", "video.mkv", 6, segment,
		[]string{"news.easynews.com", "news.easynews.com"}, []string{"easynews", "easynews"}, 767999, metadata, err)

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read failure record: %v", readErr)
	}
	var got articleFailureRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode failure record: %v", err)
	}
	if got.NZBID != "nzb-123" || got.Filename != "video.mkv" || got.SegmentIndex != 6 || got.SegmentNumber != 7 {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.MessageID != segment.MessageID || got.Group != segment.Group || got.DeclaredBytes != segment.Bytes {
		t.Fatalf("segment metadata mismatch: %+v", got)
	}
	if got.ReceivedBytes != 767999 || got.Attempts != 2 || got.Provider != "news.easynews.com" {
		t.Fatalf("attempt metadata mismatch: %+v", got)
	}
	if got.ProviderBackbone != "easynews" || got.ArticleKey != "easynews|article@example" {
		t.Fatalf("article-provider key mismatch: %+v", got)
	}
	if got.ErrorClass != "YENC_DECODE" || got.CRCResult != "mismatch" || got.YencPart != 7 || got.YencPartSize != 768000 {
		t.Fatalf("decoder metadata mismatch: %+v", got)
	}
}

func TestArticleFailureKeyFallsBackToProviderHost(t *testing.T) {
	if got := articleFailureKey("", "News.EasyNews.Com", "<ARTICLE@Example>"); got != "news.easynews.com|article@example" {
		t.Fatalf("host fallback key = %q", got)
	}
}
