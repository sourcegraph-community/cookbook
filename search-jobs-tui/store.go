// A small on-disk record of jobs this tool has created.
//
// Search Jobs outlive the process that started them, so without this you would
// lose track of a job the moment you quit. When the instance implements
// ListSearchJobs the dashboard prefers that and this cache is only a fallback.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StoredJob is the minimum needed to find a job again after a restart.
type StoredJob struct {
	Name      string    `json:"name"`
	Query     string    `json:"query"`
	CreatedAt time.Time `json:"createdAt"`
}

// DefaultStorePath is ~/.cache/sourcegraph-search-jobs/jobs.json on Linux and
// the platform equivalent elsewhere. An empty string means "could not resolve a
// cache directory", which callers treat as "run without persistence".
func DefaultStorePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sourcegraph-search-jobs", "jobs.json")
}

// LoadStore reads the cache. A missing or corrupt file is not an error worth
// interrupting anyone over; you just start with an empty dashboard.
func LoadStore(path string) []StoredJob {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var jobs []StoredJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil
	}
	return jobs
}

// SaveStore writes the cache, keeping only the most recent limit entries so the
// file cannot grow without bound.
func SaveStore(path string, jobs []StoredJob, limit int) error {
	if path == "" {
		return nil
	}
	if len(jobs) > limit {
		jobs = jobs[len(jobs)-limit:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
