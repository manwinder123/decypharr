package usenet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sourcegraph/conc/pool"
)

// segmentResult holds a fetched segment and its index for ordered writing
type segmentResult struct {
	index int
	data  []byte
	err   error
}

// ProgressCallback is called periodically during download with progress info
// downloaded: total bytes written so far, speed: bytes per second (estimated)
type ProgressCallback func(downloaded int64, speed int64)

// articleFailureRecord is intentionally append-only and bounded by
// articleFailureLogMaxBytes. It identifies one exact NZB article rather than
// treating an entire NZB or provider as bad. Partial bytes are diagnostic
// only; callers discard them whenever err is non-nil.
type articleFailureRecord struct {
	RecordedAt        time.Time `json:"recorded_at"`
	NZBID             string    `json:"nzb_id"`
	Filename          string    `json:"filename"`
	SegmentIndex      int       `json:"segment_index"`
	SegmentNumber     int       `json:"segment_number"`
	MessageID         string    `json:"message_id"`
	ArticleKey        string    `json:"article_key"`
	Group             string    `json:"group,omitempty"`
	DeclaredBytes     int64     `json:"declared_bytes"`
	SegmentDataStart  int64     `json:"segment_data_start"`
	ReceivedBytes     int64     `json:"received_bytes"`
	Attempts          int       `json:"attempts"`
	Providers         []string  `json:"providers,omitempty"`
	ProviderBackbones []string  `json:"provider_backbones,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	ProviderBackbone  string    `json:"provider_backbone,omitempty"`
	ErrorClass        string    `json:"error_class"`
	ErrorCode         int       `json:"error_code,omitempty"`
	Error             string    `json:"error"`
	CRCResult         string    `json:"crc_result"`
	YencFilename      string    `json:"yenc_filename,omitempty"`
	YencSize          int64     `json:"yenc_size,omitempty"`
	YencPart          int64     `json:"yenc_part,omitempty"`
	YencTotal         int64     `json:"yenc_total,omitempty"`
	YencBegin         int64     `json:"yenc_begin,omitempty"`
	YencEnd           int64     `json:"yenc_end,omitempty"`
	YencOffset        int64     `json:"yenc_offset,omitempty"`
	YencPartSize      int64     `json:"yenc_part_size,omitempty"`
}

const articleFailureLogMaxBytes int64 = 16 << 20

func articleProviderIdentity(backbone, host string) string {
	identity := strings.ToLower(strings.TrimSpace(backbone))
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(host))
	}
	if identity == "" {
		identity = "unknown"
	}
	return identity
}

func normalizeArticleMessageID(messageID string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(messageID)), "<>")
}

// articleFailureKey is stable across NZB submissions and indexers. Provider
// backbone is preferred so two hosts on one backbone share the same identity;
// the host is the safe fallback when no backbone is configured.
func articleFailureKey(backbone, host, messageID string) string {
	return articleProviderIdentity(backbone, host) + "|" + normalizeArticleMessageID(messageID)
}

// recordArticleFailure persists enough identity and decoder context to
// distinguish a missing article (430), a decoder/CRC mismatch, and a local
// metadata error during later replay. Failure logging must never change the
// download result.
func (u *Usenet) recordArticleFailure(nzoID, filename string, segmentIndex int, segment storage.NZBSegment,
	providers, backbones []string, receivedBytes int64, metadata *nntp.YencMetadata, err error) {
	if u == nil || u.failureLogPath == "" || err == nil {
		return
	}

	class := "unknown"
	code := 0
	var nntpErr *nntp.Error
	if errors.As(err, &nntpErr) {
		class = nntpErr.Type.String()
		code = nntpErr.Code
	} else {
		class = "local_validation"
	}

	message := strings.ToLower(err.Error())
	crcResult := "not_applicable"
	if strings.Contains(message, "crc32 mismatch") {
		crcResult = "mismatch"
	} else if class == "YENC_DECODE" {
		crcResult = "not_checked"
	}

	record := articleFailureRecord{
		RecordedAt:       time.Now().UTC(),
		NZBID:            nzoID,
		Filename:         filename,
		SegmentIndex:     segmentIndex,
		SegmentNumber:    segment.Number,
		MessageID:        segment.MessageID,
		ArticleKey:       articleFailureKey("", "", segment.MessageID),
		Group:            segment.Group,
		DeclaredBytes:    segment.Bytes,
		SegmentDataStart: segment.SegmentDataStart,
		ReceivedBytes:    receivedBytes,
		Attempts:         len(providers),
		ErrorClass:       class,
		ErrorCode:        code,
		Error:            err.Error(),
		CRCResult:        crcResult,
	}
	if len(providers) > 0 {
		record.Providers = append([]string(nil), providers...)
		record.Provider = providers[len(providers)-1]
		record.ArticleKey = articleFailureKey("", record.Provider, record.MessageID)
		if len(backbones) > len(providers)-1 {
			record.ProviderBackbones = append([]string(nil), backbones...)
			record.ProviderBackbone = articleProviderIdentity(backbones[len(providers)-1], record.Provider)
			record.ArticleKey = articleFailureKey(record.ProviderBackbone, record.Provider, record.MessageID)
		}
	}
	if metadata != nil {
		record.YencFilename = metadata.Name
		record.YencSize = metadata.Size
		record.YencPart = metadata.Part
		record.YencTotal = metadata.Total
		record.YencBegin = metadata.Begin
		record.YencEnd = metadata.End
		record.YencOffset = metadata.Offset
		record.YencPartSize = metadata.PartSize
	}

	line, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		u.logger.Warn().Err(marshalErr).Msg("Failed to encode article failure record")
		return
	}
	line = append(line, '\n')

	u.failureLogMu.Lock()
	defer u.failureLogMu.Unlock()
	if info, statErr := os.Stat(u.failureLogPath); statErr == nil && info.Size()+int64(len(line)) > articleFailureLogMaxBytes {
		// Keep one previous generation for incident replay while bounding disk
		// growth. A failure record is evidence, not a retry queue.
		_ = os.Remove(u.failureLogPath + ".1")
		if renameErr := os.Rename(u.failureLogPath, u.failureLogPath+".1"); renameErr != nil {
			u.logger.Warn().Err(renameErr).Msg("Failed to rotate article failure log")
		}
	}
	f, openErr := os.OpenFile(u.failureLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if openErr != nil {
		u.logger.Warn().Err(openErr).Msg("Failed to open article failure log")
		return
	}
	if _, writeErr := f.Write(line); writeErr != nil {
		u.logger.Warn().Err(writeErr).Msg("Failed to append article failure record")
	}
	if syncErr := f.Sync(); syncErr != nil {
		u.logger.Debug().Err(syncErr).Msg("Failed to sync article failure record")
	}
	if closeErr := f.Close(); closeErr != nil {
		u.logger.Debug().Err(closeErr).Msg("Failed to close article failure log")
	}
}

// Download downloads a file by fetching segments in parallel and streaming to writer in order.
// Bytes flow to the writer progressively as in-order segments complete - no waiting for all segments.
// If progressCallback is provided, it will be called after each segment write with current progress.
func (u *Usenet) Download(ctx context.Context, nzoID, filename string, writer io.Writer, progressCallback ProgressCallback) error {
	// get file metadata
	file, err := u.getFile(nzoID, filename)
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}

	if len(file.Segments) == 0 {
		return fmt.Errorf("file has no segments: %s", file.Name)
	}

	// Track progress
	var completedSegments atomic.Int64
	var downloadedBytes atomic.Int64

	// Channel for segment results - buffered to allow parallel fetching ahead
	resultChan := make(chan segmentResult, max(u.processingMaxConnections, 1)*2)

	// Map to hold out-of-order segments waiting to be written
	pendingSegments := make(map[int][]byte)
	var pendingMu sync.Mutex
	nextToWrite := 0

	// Error tracking
	var writeErr error
	var writeErrMu sync.Mutex

	// Writer goroutine - writes segments in order as they arrive
	var writerWg sync.WaitGroup
	writerWg.Go(func() {
		for result := range resultChan {
			if result.err != nil {
				writeErrMu.Lock()
				if writeErr == nil {
					writeErr = result.err
				}
				writeErrMu.Unlock()
				continue
			}

			pendingMu.Lock()
			pendingSegments[result.index] = result.data

			// Write all consecutive segments starting from nextToWrite
			for {
				data, exists := pendingSegments[nextToWrite]
				if !exists {
					break
				}
				delete(pendingSegments, nextToWrite)
				pendingMu.Unlock()

				// Write to output
				n, err := writer.Write(data)
				if err != nil {
					writeErrMu.Lock()
					if writeErr == nil {
						writeErr = fmt.Errorf("write failed at segment %d: %w", nextToWrite, err)
					}
					writeErrMu.Unlock()
					pendingMu.Lock()
					break
				}

				completedSegments.Add(1)
				downloaded := downloadedBytes.Add(int64(n))
				nextToWrite++

				// Call progress callback if provided
				if progressCallback != nil {
					// Estimate speed (rough: assume ~1s per segment batch)
					completed := completedSegments.Load()
					speed := downloaded / max(1, completed) * int64(max(u.processingMaxConnections, 1))
					progressCallback(downloaded, speed)
				}

				pendingMu.Lock()
			}
			pendingMu.Unlock()
		}
	})

	// Fetch segments in parallel
	p := pool.New().WithContext(ctx).WithMaxGoroutines(max(u.processingMaxConnections, 1))

	for idx, segment := range file.Segments {
		segIdx := idx
		seg := segment

		p.Go(func(ctx context.Context) error {
			// Check for write errors
			writeErrMu.Lock()
			if writeErr != nil {
				writeErrMu.Unlock()
				return writeErr
			}
			writeErrMu.Unlock()

			// Check context
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Fetch segment using manager with failover. Keep the article identity
			// and decoder metadata from every attempt so a later replay can tell
			// missing content from transport/decoder corruption.
			var data []byte
			var metadata *nntp.YencMetadata
			var providers []string
			var backbones []string
			var receivedBytes int64
			recordFailure := func(failure error) {
				u.recordArticleFailure(nzoID, filename, segIdx, seg, providers, backbones, receivedBytes, metadata, failure)
			}
			err := u.nntp.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
				d, m, e := conn.GetDecodedBodyWithMetadata(seg.MessageID)
				data = d
				metadata = m
				receivedBytes = int64(len(d))
				providers = append(providers, conn.ProviderHost())
				backbones = append(backbones, conn.ProviderBackbone())
				return e
			})
			if err != nil {
				failure := fmt.Errorf("segment %d: %w", segIdx, err)
				recordFailure(failure)
				if nntp.IsArticleNotFoundError(err) {
					// This is definitive only after ExecuteWithFailover has
					// exhausted every configured provider. Persist the exact
					// logical file quarantine; do not poison the whole NZB.
					u.failedFiles.Store(fsKey(nzoID, filename), err)
					u.markNZBFileDeleted(nzoID, filename)
				}
				resultChan <- segmentResult{index: segIdx, err: failure}
				return nil // Don't stop other workers
			}

			// Handle SegmentDataStart for sliced segments.
			// Guard against malformed negative or out-of-range offsets
			// (a negative SegmentDataStart used to panic the whole process
			// via a slice out-of-range).
			if seg.SegmentDataStart > 0 {
				if seg.SegmentDataStart >= int64(len(data)) {
					failure := fmt.Errorf("segment %d: offset exceeds data", segIdx)
					recordFailure(failure)
					resultChan <- segmentResult{index: segIdx, err: failure}
					return nil
				}
				data = data[seg.SegmentDataStart:]
			}

			// Trim to expected size. seg.Bytes must be positive: a malformed
			// expected size used to panic here with a negative slice bound.
			if seg.Bytes <= 0 {
				failure := fmt.Errorf("segment %d: invalid size %d", segIdx, seg.Bytes)
				recordFailure(failure)
				resultChan <- segmentResult{index: segIdx, err: failure}
				return nil
			}
			if int64(len(data)) > seg.Bytes {
				data = data[:seg.Bytes]
			}

			resultChan <- segmentResult{index: segIdx, data: data}
			return nil
		})
	}

	// Wait for all fetches to complete, then close result channel
	fetchErr := p.Wait()
	close(resultChan)

	// Wait for writer to finish
	writerWg.Wait()

	// Check for errors
	if writeErr != nil {
		return writeErr
	}
	if fetchErr != nil {
		return fetchErr
	}

	u.logger.Info().
		Str("file", filename).
		Int64("bytes", downloadedBytes.Load()).
		Msg("Download complete")

	return nil
}
