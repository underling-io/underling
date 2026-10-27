// Package github opens draft pull requests through the GitHub REST API.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	errorBodyMax    = 512
	responseBodyMax = 1 << 20
)

// Client opens pull requests.
type Client struct {
	APIURL string // the REST API base, e.g. https://api.github.com
	Token  string
	HTTP   *http.Client
}

// PullRequest is a pull request to open.
type PullRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// OpenDraftPR opens pr as a draft in owner/name and returns its URL. Any
// answer but 201 is an error naming the status and the start of the body.
func (c *Client) OpenDraftPR(ctx context.Context, owner, name string, pr PullRequest) (string, error) {
	pr.Draft = true
	payload, err := json.Marshal(pr)
	if err != nil {
		return "", err
	}
	url := strings.TrimSuffix(c.APIURL, "/") + "/repos/" + owner + "/" + name + "/pulls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyMax))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		excerpt := strings.TrimSpace(string(body))
		return "", fmt.Errorf("create pull request: status %d: %s", resp.StatusCode, excerpt[:min(len(excerpt), errorBodyMax)])
	}
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal(body, &created) != nil || created.HTMLURL == "" {
		return "", errors.New("create pull request: no html_url in response")
	}
	return created.HTMLURL, nil
}
