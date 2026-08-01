// Search Jobs API client.
//
// This file is the entire HTTP surface of the recipe. If you came for the API
// and not the terminal UI, read this file and stop. Nothing here imports Bubble
// Tea, and nothing here draws anything.
//
//	POST /api/searchjobs.v1.Service/CreateSearchJob   (scope: externalapi:write)
//	POST /api/searchjobs.v1.Service/GetSearchJob      (scope: externalapi:read)
//	POST /api/searchjobs.v1.Service/ListSearchJobs    (scope: externalapi:read)
//	POST /api/searchjobs.v1.Service/CancelSearchJob   (scope: externalapi:write)
//	GET  <SearchJob.resultsUrl>                       -> JSONL

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Job states, as returned in SearchJob.State.
const (
	StateUnspecified = "STATE_UNSPECIFIED"
	StateQueued      = "STATE_QUEUED"
	StateProcessing  = "STATE_PROCESSING"
	StateCompleted   = "STATE_COMPLETED"
	StateErrored     = "STATE_ERRORED"
	StateFailed      = "STATE_FAILED"
	StateCanceled    = "STATE_CANCELED"
)

// SearchJob mirrors searchjobs.v1.SearchJob.
type SearchJob struct {
	Name       string `json:"name"` // users/{user}/searchJobs/{id}
	Query      string `json:"query"`
	State      string `json:"state"`
	CreateTime string `json:"createTime,omitempty"`
	StartTime  string `json:"startTime,omitempty"`
	ResultsURL string `json:"resultsUrl,omitempty"`
	LogsURL    string `json:"logsUrl,omitempty"`
}

// IsTerminal reports whether a state will never change again, which is what
// takes a job out of the poll set.
func IsTerminal(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled:
		return true
	}
	return false
}

// PrettyState turns STATE_PROCESSING into "processing".
func PrettyState(state string) string {
	return strings.ToLower(strings.TrimPrefix(state, "STATE_"))
}

// Client talks to one Sourcegraph instance.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

func NewClient(endpoint, token string) *Client {
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Token:    token,
		// No overall timeout: a results download can legitimately run for a
		// long time. Cancellation is per-request, through the context.
		HTTP: &http.Client{},
	}
}

// APIError is a non-2xx response, with the server's message when it sent one.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		if e.Code != "" {
			return fmt.Sprintf("HTTP %d: %s (%s)", e.Status, e.Message, e.Code)
		}
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Unsupported reports whether the instance does not implement a method. Older
// instances predate some of these RPCs, so the dashboard hides the features
// they back rather than failing.
func Unsupported(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusNotImplemented {
		return true
	}
	switch strings.ToLower(apiErr.Code) {
	case "unimplemented", "not_found":
		return true
	}
	return false
}

// Unauthorized reports whether the token was rejected, which is worth saying
// plainly because it is the most common setup mistake.
func Unauthorized(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
}

func errorFromResponse(res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	apiErr := &APIError{Status: res.StatusCode}
	var parsed struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		apiErr.Code, apiErr.Message = parsed.Code, parsed.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr
}

// rpc posts a JSON body to one ConnectRPC method and decodes the reply.
func (c *Client) rpc(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	endpoint := c.Endpoint + "/api/searchjobs.v1.Service/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return errorFromResponse(res)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// CreateSearchJob starts a job. The parent "users/-" means the authenticated user.
func (c *Client) CreateSearchJob(ctx context.Context, query string) (*SearchJob, error) {
	in := map[string]any{
		"parent":    "users/-",
		"searchJob": map[string]string{"query": query},
	}
	var out SearchJob
	if err := c.rpc(ctx, "CreateSearchJob", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSearchJob fetches one job's current state.
func (c *Client) GetSearchJob(ctx context.Context, name string) (*SearchJob, error) {
	var out SearchJob
	if err := c.rpc(ctx, "GetSearchJob", map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSearchJobs returns the caller's existing jobs.
//
// Best-effort: instances that do not implement it return an error satisfying
// Unsupported, and the dashboard falls back to its local cache. The response
// field name here follows the AIP list convention used elsewhere in the API;
// confirm it against your instance's OpenAPI schema at /api-reference if this
// ever comes back empty on an instance that clearly has jobs.
func (c *Client) ListSearchJobs(ctx context.Context) ([]*SearchJob, error) {
	var out struct {
		SearchJobs []*SearchJob `json:"searchJobs"`
	}
	if err := c.rpc(ctx, "ListSearchJobs", map[string]string{"parent": "users/-"}, &out); err != nil {
		return nil, err
	}
	return out.SearchJobs, nil
}

// CancelSearchJob stops a running job. Best-effort, like ListSearchJobs.
func (c *Client) CancelSearchJob(ctx context.Context, name string) error {
	return c.rpc(ctx, "CancelSearchJob", map[string]string{"name": name}, nil)
}

// ResolveURL turns a SearchJob's resultsUrl or logsUrl into an absolute URL.
//
// Instances return these absolute today, but the field is a URL reference, so
// resolve instead of concatenating. Gluing the endpoint onto an already
// absolute URL produces https://host/https:/host/api/... .
func (c *Client) ResolveURL(ref string) (string, error) {
	base, err := url.Parse(c.Endpoint + "/")
	if err != nil {
		return "", err
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(u).String(), nil
}

// SearchJobsPageURL is the web UI's Search Jobs list.
//
// This, not resultsUrl, is what "open in a browser" has to point at. resultsUrl
// and logsUrl live under /api/, the external API, which accepts only an
// Authorization header. A browser sends a session cookie instead, so a
// signed-in user still gets "External API requires authentication." The web UI
// page lists the same jobs with download links that do work off the session.
func (c *Client) SearchJobsPageURL() string {
	return c.Endpoint + "/search-jobs"
}

// DownloadStats is what a finished download produced.
type DownloadStats struct {
	Lines int64
	Bytes int64
}

// DownloadResults streams a completed job's JSONL to outPath.
//
// onProgress is called with the bytes written so far and the total the server
// declared, or -1 when it streamed without a Content-Length. It is throttled to
// roughly 20 calls a second and always fires once more at the end. The body is
// never held in memory: a result set can be hundreds of megabytes.
func (c *Client) DownloadResults(
	ctx context.Context,
	resultsURL, outPath string,
	onProgress func(done, total int64),
) (DownloadStats, error) {
	var stats DownloadStats

	target, err := c.ResolveURL(resultsURL)
	if err != nil {
		return stats, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return stats, err
	}
	req.Header.Set("Authorization", "token "+c.Token)

	res, err := c.HTTP.Do(req)
	if err != nil {
		return stats, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return stats, errorFromResponse(res)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return stats, err
	}
	defer f.Close()
	out := bufio.NewWriterSize(f, 256<<10)

	total := res.ContentLength // -1 when the server did not declare one
	report := func() {
		if onProgress != nil {
			onProgress(stats.Bytes, total)
		}
	}

	buf := make([]byte, 128<<10)
	// Seeded to '\n' so an empty body counts zero lines rather than one.
	lastByte := byte('\n')
	lastReport := time.Now()

	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := out.Write(chunk); err != nil {
				return stats, err
			}
			stats.Bytes += int64(n)
			stats.Lines += int64(bytes.Count(chunk, []byte{'\n'}))
			lastByte = chunk[n-1]
			if time.Since(lastReport) >= 50*time.Millisecond {
				lastReport = time.Now()
				report()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return stats, readErr
		}
	}

	// A final line with no trailing newline still counts.
	if lastByte != '\n' {
		stats.Lines++
	}
	if err := out.Flush(); err != nil {
		return stats, err
	}
	report()
	return stats, nil
}
