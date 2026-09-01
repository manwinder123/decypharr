package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DMMClient calls Debrid Media Manager's public debrid-uploader bridge.
// This is the same flow as the Patreon post "Send torrents TB"
// (https://www.patreon.com/debridmediamanager/posts/send-torrents-tb-165863827):
// DMM's debrid02/debrid01 servers rebuild the torrent as a webseed torrent
// with de-infringed filenames and add it to the caller's RD account, sourcing
// bytes from TorBox/AllDebrid via the webseed. The server farms the bandwidth,
// the caller's RD account gets the cache. No local public webseed required.
type DMMClient struct {
	BaseURL string // default https://debridmediamanager.com
	HTTP    *http.Client
}

func NewDMMClient(baseURL string) *DMMClient {
	if baseURL == "" {
		baseURL = "https://debridmediamanager.com"
	}
	return &DMMClient{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type CreateJobRequest struct {
	// Hash is the 40-char infohash cached on TorBox/AllDebrid but 451 on RD
	Hash string `json:"hash"`
	// ImdbID required by DMM for availability registration (e.g. "tt1234567")
	ImdbID string `json:"imdbId"`
	// RdKey is the caller's Real-Debrid API key that will own the rewritten torrent
	RdKey string `json:"rdKey"`
	// TbKey is optional TorBox API key for sourcing bytes
	TbKey string `json:"tbKey,omitempty"`
	// AdKey is optional AllDebrid API key
	AdKey string `json:"adKey,omitempty"`
	// SizeBytes hints routing (large torrents avoid capped hosts)
	SizeBytes *int64 `json:"sizeBytes,omitempty"`
}

type CreateJobResponse struct {
	// Success case
	ID string `json:"id,omitempty"`
	// Duplicate case (already transferred by another user)
	Duplicate     string  `json:"duplicate,omitempty"` // "completed" | "in_progress"
	RewrittenHash *string `json:"rewrittenHash,omitempty"`
	JobID         string  `json:"jobId,omitempty"`
	AddedToRd     bool    `json:"addedToRd,omitempty"`
	// Error
	Error string `json:"error,omitempty"`
}

// CreateJob submits a hash to DMM's debrid-uploader. If a completed duplicate
// exists, it returns Duplicate=="completed" and the rewritten hash is already
// RD-cached — the caller should add that hash to RD via addMagnet directly
// for instant availability (no webseed fetch needed).
func (c *DMMClient) CreateJob(req CreateJobRequest) (*CreateJobResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", c.BaseURL+"/api/debrid-uploader/jobs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dmm bridge: %w", err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var out CreateJobResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("dmm bridge decode: status %d body %q: %w", resp.StatusCode, string(b), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Error != "" {
			return &out, fmt.Errorf("dmm bridge %d: %s", resp.StatusCode, out.Error)
		}
		return &out, fmt.Errorf("dmm bridge status %d: %s", resp.StatusCode, string(b))
	}
	return &out, nil
}

// JobStatus is the polled status of a bridge job.
type JobStatus struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // pending, downloading, preparing, uploading, completed, failed
	RewrittenHash *string `json:"rewrittenHash,omitempty"`
	Error         *string `json:"error,omitempty"`
}

// GetJob fetches current job status.
func (c *DMMClient) GetJob(jobID string) (*JobStatus, error) {
	httpReq, err := http.NewRequest("GET", c.BaseURL+"/api/debrid-uploader/jobs/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("dmm bridge get job %d: %s", resp.StatusCode, string(b))
	}
	var out JobStatus
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollUntilComplete polls until terminal status or timeout.
func (c *DMMClient) PollUntilComplete(jobID string, timeout time.Duration) (*JobStatus, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		js, err := c.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		if js.Status == "completed" || js.Status == "failed" {
			return js, nil
		}
		time.Sleep(5 * time.Second)
	}
	return nil, fmt.Errorf("dmm bridge poll timeout for %s", jobID)
}
