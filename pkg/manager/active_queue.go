package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

func (m *Manager) restoreActiveDownloadJobs() {
	entries := m.queue.ListFilter("", config.ProtocolAll, storage.EntryStateDownloading, nil, "", false)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].AddedOn.Before(entries[j].AddedOn)
	})

	// NZB entries are owned by the local worker pool. Torrent entries can use
	// a lightweight wait job while the regular queue processor polls the
	// provider, but an NZB wait job has no external actor that can advance it.
	// In particular, a completed-but-not-finalized NZB used to occupy a worker
	// forever via processJob's wait path, starving every genuinely active NZB
	// after a restart.
	for _, entry := range entries {
		if entry.IsNZB() {
			continue
		}
		if entry.Status == debridTypes.TorrentStatusQueued || m.nzbNeedsReprocessing(entry) {
			continue
		}
		_ = m.SubmitJob(&Job{
			ID:    entry.InfoHash,
			Type:  jobTypeForEntry(entry),
			Entry: entry,
		})
	}

	// Rebuild interrupted NZB parsing jobs, and resume NZBs whose post-parse
	// action was interrupted after processAction marked the queue status as
	// downloaded. The latter must not go through the wait-only job above.
	for _, entry := range entries {
		if entry == nil {
			continue
		}

		var (
			job *Job
			err error
		)
		if entry.IsNZB() {
			switch {
			case m.nzbNeedsReprocessing(entry):
				job, err = m.rebuildQueuedNZBJob(entry)
			case !entry.IsComplete &&
				(entry.Status == debridTypes.TorrentStatusDownloaded || entry.Status == debridTypes.TorrentStatusDownloading) &&
				len(entry.Files) > 0:
				// processAction may have persisted the entry before its local
				// download completed. Re-run the action using the persisted file
				// list; this is resumable at the job level and, unlike waiting,
				// makes forward progress.
				job = &Job{
					ID:             entry.InfoHash,
					Type:           JobTypeNZB,
					Entry:          entry,
					ResumeExisting: true,
				}
			case !entry.IsComplete && entry.Status == debridTypes.TorrentStatusDownloading:
				// A metadata/header race can leave an active NZB with no files
				// in the queue entry. Reparse the retained source if possible;
				// a real error is preferable to an immortal 0% row.
				job, err = m.rebuildQueuedNZBJob(entry)
			default:
				continue
			}
		} else {
			if entry.Status != debridTypes.TorrentStatusQueued && !m.nzbNeedsReprocessing(entry) {
				continue
			}
			job, err = m.rebuildQueuedJob(entry)
		}

		if err != nil {
			entry.IsDownloading = false
			entry.MarkAsError(err)
			_ = m.queue.Update(entry)
			continue
		}
		if job == nil {
			continue
		}

		// Fence the NZB from processQueuedEntries while its restore job is
		// queued. This flag is cleared by the normal completion/error paths.
		if entry.IsNZB() {
			entry.IsDownloading = true
		}
		if job.DebridTorrent == nil && job.NZBMeta == nil && !job.ResumeExisting {
			entry.Status = debridTypes.TorrentStatusQueued
		}
		_ = m.queue.Update(entry)
		if err := m.SubmitJob(job); err != nil {
			entry.IsDownloading = false
			entry.MarkAsError(err)
			_ = m.queue.Update(entry)
		}
	}
}

func jobTypeForEntry(entry *storage.Entry) JobType {
	if entry != nil && entry.IsNZB() {
		return JobTypeNZB
	}
	return JobTypeTorrent
}

func (m *Manager) nzbNeedsReprocessing(entry *storage.Entry) bool {
	if entry == nil || !entry.IsNZB() || m.usenet == nil {
		return false
	}
	meta, err := m.usenet.GetNZBHeader(entry.InfoHash)
	return err == nil && meta != nil && (meta.Status == usenet.NZBStatusParsing || meta.Status == usenet.NZBStatusDownloading)
}

func (m *Manager) rebuildQueuedJob(entry *storage.Entry) (*Job, error) {
	if entry.IsNZB() {
		return m.rebuildQueuedNZBJob(entry)
	}
	return m.rebuildQueuedTorrentJob(entry)
}

func (m *Manager) rebuildQueuedTorrentJob(entry *storage.Entry) (*Job, error) {
	if entry.ActiveProvider != "" && entry.GetActiveProvider() != nil {
		return &Job{
			ID:             entry.InfoHash,
			Type:           JobTypeTorrent,
			Entry:          entry,
			ResumeExisting: true,
		}, nil
	}

	magnet, err := utils.GetMagnetInfo(entry.Magnet, m.config.AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}

	downloadUncached := entry.DownloadUncached
	req := NewTorrentRequest(
		entry.ActiveProvider,
		downloadFolderForEntry(m.config.DownloadFolder, entry),
		magnet,
		m.arr.GetOrCreate(entry.Category),
		entry.Action,
		&downloadUncached,
		entry.CallbackURL,
		ImportTypeAPI,
		entry.SkipMultiSeason,
	)
	req.Id = entry.InfoHash
	job := NewJob(JobTypeTorrent, req)
	job.ID = entry.InfoHash
	job.Entry = entry
	return job, nil
}

func (m *Manager) rebuildQueuedNZBJob(entry *storage.Entry) (*Job, error) {
	if m.usenet == nil {
		return nil, fmt.Errorf("usenet is not configured")
	}
	sourcePath := entry.Magnet
	if meta, err := m.usenet.GetNZBHeader(entry.InfoHash); err == nil && meta != nil && meta.Path != "" {
		sourcePath = meta.Path
	}
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}

	name := entry.OriginalFilename
	if name == "" {
		name = entry.Name
	}
	meta, groups, err := m.usenet.ParseWithID(context.Background(), entry.InfoHash, name, content, entry.Category)
	if err != nil {
		return nil, fmt.Errorf("usenet parse failed: %w", err)
	}
	if entry.Magnet != "" && sourcePath == entry.Magnet {
		m.usenet.RemoveStagedNZB(entry.Magnet)
	}

	entry.Magnet = ""
	entry.Name = meta.Name
	entry.OriginalFilename = meta.Name
	entry.Size = meta.TotalSize
	entry.Bytes = meta.TotalSize
	entry.Status = debridTypes.TorrentStatusDownloading
	entry.ActiveProvider = "usenet"
	_ = entry.AddUsenetProvider(meta)

	req := NewNZBRequest(
		meta.Name,
		downloadFolderForEntry(m.config.DownloadFolder, entry),
		content,
		m.arr.GetOrCreate(entry.Category),
		entry.Action,
		entry.CallbackURL,
		ImportTypeSABnzbd,
		entry.SkipMultiSeason,
	)
	req.Id = entry.InfoHash
	job := NewJob(JobTypeNZB, req)
	job.ID = entry.InfoHash
	job.Entry = entry
	job.NZBMeta = meta
	job.NZBGroups = groups
	return job, nil
}

func downloadFolderForEntry(fallback string, entry *storage.Entry) string {
	if entry != nil && entry.SavePath != "" {
		return filepath.Dir(entry.SavePath)
	}
	return fallback
}
