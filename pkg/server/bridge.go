package server

import (
	"encoding/json"
	"net/http"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/bridge"
)

// handleBridgeCreate proxies a TorBox->RD bridge request via DMM or local webseed.
// Request body: { hash, imdbId, sizeBytes? }
// Uses the caller's configured debrid API keys (RD + TorBox/AD) from config.
func (s *Server) handleBridgeCreate(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	if !cfg.Bridge.Enabled {
		http.Error(w, "bridge disabled (enable in config.json bridge.enabled)", http.StatusBadRequest)
		return
	}

	var req struct {
		Hash      string `json:"hash"`
		ImdbID    string `json:"imdbId"`
		SizeBytes *int64 `json:"sizeBytes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Hash) != 40 {
		http.Error(w, "hash must be 40-char hex", http.StatusBadRequest)
		return
	}
	if req.ImdbID == "" {
		req.ImdbID = "tt0000000" // placeholder; DMM requires it for registration
	}

	// Find RD and source keys from config
	var rdKey, tbKey, adKey string
	for _, d := range cfg.Debrids {
		switch d.Provider {
		case "realdebrid":
			rdKey = d.APIKey
		case "torbox":
			tbKey = d.APIKey
		case "alldebrid":
			adKey = d.APIKey
		}
	}
	if rdKey == "" {
		http.Error(w, "no realdebrid provider configured", http.StatusBadRequest)
		return
	}
	if tbKey == "" && adKey == "" {
		http.Error(w, "no torbox/alldebrid source configured", http.StatusBadRequest)
		return
	}

	if cfg.Bridge.UseDMM {
		client := bridge.NewDMMClient(cfg.Bridge.DMMBaseURL)
		out, err := client.CreateJob(bridge.CreateJobRequest{
			Hash:      req.Hash,
			ImdbID:    req.ImdbID,
			RdKey:     rdKey,
			TbKey:     tbKey,
			AdKey:     adKey,
			SizeBytes: req.SizeBytes,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	// Local webseed path: not yet implemented as a self-hosted webseed server
	// requires WebseedPublicURL to be reachable by Real-Debrid.
	if cfg.Bridge.WebseedPublicURL == "" {
		http.Error(w, "bridge.use_dmm is false but bridge.webseed_public_url is not set; set it to a public URL or enable use_dmm", http.StatusBadRequest)
		return
	}
	http.Error(w, "local webseed bridge not yet implemented; enable bridge.use_dmm to use DMM's hosted webseed", http.StatusNotImplemented)
}

func (s *Server) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	if !cfg.Bridge.Enabled || !cfg.Bridge.UseDMM {
		http.Error(w, "bridge DMM polling only available with bridge.enabled && bridge.use_dmm", http.StatusBadRequest)
		return
	}
	jobID := r.PathValue("id")
	if jobID == "" {
		jobID = r.URL.Query().Get("id")
	}
	// chi version in go.mod may not support PathValue; try chi URL param
	if jobID == "" {
		// fallback: extract from URL path after last /
		p := r.URL.Path
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' {
				jobID = p[i+1:]
				break
			}
		}
	}
	if jobID == "" {
		http.Error(w, "job id required", http.StatusBadRequest)
		return
	}
	client := bridge.NewDMMClient(cfg.Bridge.DMMBaseURL)
	js, err := client.GetJob(jobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(js)
}
