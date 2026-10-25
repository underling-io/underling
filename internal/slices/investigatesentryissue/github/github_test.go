package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeAPI answers every request with status and body, and keeps the last
// request and its body.
type fakeAPI struct {
	status int
	body   string
	req    *http.Request
	sent   []byte
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.req = r
	f.sent, _ = io.ReadAll(r.Body)
	return &http.Response{
		StatusCode: f.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    r,
	}, nil
}

func client(api *fakeAPI) *Client {
	return &Client{APIURL: "https://api.github.test/", Token: "gh-token", HTTP: &http.Client{Transport: api}}
}

func TestOpenDraftPRPostsADraftAndReturnsItsURL(t *testing.T) {
	api := &fakeAPI{status: http.StatusCreated, body: `{"number":7,"html_url":"https://github.com/acme/web/pull/7"}`}

	url, err := client(api).OpenDraftPR(context.Background(), "acme", "web", PullRequest{Title: "Fix", Head: "heinzel/sentry-1", Base: "main", Body: "body"})

	if err != nil || url != "https://github.com/acme/web/pull/7" {
		t.Fatalf("got (%q, %v), want the PR URL", url, err)
	}
	if api.req.Method != http.MethodPost || api.req.URL.String() != "https://api.github.test/repos/acme/web/pulls" {
		t.Errorf("got %s %s", api.req.Method, api.req.URL)
	}
	for header, want := range map[string]string{
		"Authorization":        "Bearer gh-token",
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
		"Content-Type":         "application/json",
	} {
		if got := api.req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	var sent PullRequest
	if err := json.Unmarshal(api.sent, &sent); err != nil {
		t.Fatalf("decode %s: %v", api.sent, err)
	}
	if want := (PullRequest{Title: "Fix", Head: "heinzel/sentry-1", Base: "main", Body: "body", Draft: true}); sent != want {
		t.Errorf("sent %+v, want %+v", sent, want)
	}
}

func TestOpenDraftPRFailsOnAnyOtherStatus(t *testing.T) {
	api := &fakeAPI{status: http.StatusUnprocessableEntity, body: `{"message":"Validation Failed"}` + strings.Repeat("x", 1000)}

	_, err := client(api).OpenDraftPR(context.Background(), "acme", "web", PullRequest{})

	if err == nil || !strings.Contains(err.Error(), "status 422: {\"message\":\"Validation Failed\"}") {
		t.Fatalf("got %v, want an error with the status and the body", err)
	}
	if len(err.Error()) > len("create pull request: status 422: ")+errorBodyMax {
		t.Errorf("error holds more than %d bytes of the body: %d", errorBodyMax, len(err.Error()))
	}
}

func TestOpenDraftPRFailsWithoutAURL(t *testing.T) {
	api := &fakeAPI{status: http.StatusCreated, body: `{"number":7}`}

	_, err := client(api).OpenDraftPR(context.Background(), "acme", "web", PullRequest{})

	if err == nil || !strings.Contains(err.Error(), "no html_url") {
		t.Fatalf("got %v, want an error about the missing html_url", err)
	}
}
