package investigatesentryissue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const githubErrorBodyMax = 512

// github opens pull requests through the GitHub REST API.
type github struct {
	apiURL string
	token  string
	client *http.Client
}

type pullRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// openDraftPR opens pr as a draft in r and returns its URL. Any status but
// 201 is an error with the status and the start of the response body.
func (g *github) openDraftPR(ctx context.Context, r repo, pr pullRequest) (string, error) {
	pr.Draft = true
	body, err := json.Marshal(pr)
	if err != nil {
		return "", err
	}
	url := strings.TrimSuffix(g.apiURL, "/") + "/repos/" + r.owner + "/" + r.name + "/pulls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		excerpt := strings.TrimSpace(string(respBody))
		if len(excerpt) > githubErrorBodyMax {
			excerpt = excerpt[:githubErrorBodyMax]
		}
		return "", fmt.Errorf("create pull request: status %d: %s", resp.StatusCode, excerpt)
	}
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil || created.HTMLURL == "" {
		return "", fmt.Errorf("create pull request: no html_url in response")
	}
	return created.HTMLURL, nil
}
