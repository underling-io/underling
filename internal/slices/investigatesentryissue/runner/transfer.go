package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	// transferTimeout caps one transfer, retries included.
	transferTimeout = 10 * time.Minute
	// transferAttempts is how often a transfer is tried: again after a
	// transport error or a 5xx answer.
	transferAttempts = 3
	// defaultRetryDelay is the wait before the second attempt; the third
	// waits twice as long.
	defaultRetryDelay = time.Second
)

// errTooLarge is what a download past its limit fails with.
var errTooLarge = errors.New("larger than the limit")

// transfers moves the run's files over HTTP. Its errors never hold a URL:
// a presigned URL's query is a credential.
type transfers struct {
	client     *http.Client
	retryDelay time.Duration
}

// download GETs rawURL into the file at path.
func (t transfers) download(ctx context.Context, rawURL, path string) error {
	return t.do(ctx, http.MethodGet, rawURL, nil, func(body io.Reader) error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, body); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}

// downloadBytes GETs rawURL, which may hold at most limit bytes.
func (t transfers) downloadBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	var data []byte
	err := t.do(ctx, http.MethodGet, rawURL, nil, func(body io.Reader) error {
		var err error
		if data, err = io.ReadAll(io.LimitReader(body, limit+1)); err != nil {
			return err
		}
		if int64(len(data)) > limit {
			return errTooLarge
		}
		return nil
	})
	return data, err
}

// upload PUTs the file at path to rawURL.
func (t transfers) upload(ctx context.Context, rawURL, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return t.do(ctx, http.MethodPut, rawURL, &payload{size: info.Size(), open: func() (io.ReadCloser, error) { return os.Open(path) }}, nil)
}

// uploadBytes PUTs data to rawURL.
func (t transfers) uploadBytes(ctx context.Context, rawURL string, data []byte) error {
	return t.do(ctx, http.MethodPut, rawURL, &payload{size: int64(len(data)), open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}}, nil)
}

// payload is a request body that can be sent again.
type payload struct {
	size int64
	open func() (io.ReadCloser, error)
}

// do sends the request until it succeeds, fails for good or ran out of
// attempts. sink, if set, reads each 2xx answer's body from its start.
func (t transfers) do(ctx context.Context, method, rawURL string, body *payload, sink func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	delay := t.retryDelay
	if delay <= 0 {
		delay = defaultRetryDelay
	}
	for attempt := 1; ; attempt++ {
		retry, err := t.once(ctx, method, rawURL, body, sink)
		if err == nil {
			return nil
		}
		err = fmt.Errorf("%s: %w", method, err)
		if !retry || attempt == transferAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * delay):
		}
	}
}

// once sends the request once. It reports whether a failure is worth
// another attempt.
func (t transfers) once(ctx context.Context, method, rawURL string, body *payload, sink func(io.Reader) error) (retry bool, _ error) {
	var reader io.ReadCloser
	if body != nil {
		var err error
		if reader, err = body.open(); err != nil {
			return false, err
		}
	}
	// A typed nil must not reach NewRequest as a non-nil io.Reader.
	var requestBody io.Reader
	if reader != nil {
		requestBody = reader
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, requestBody)
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return false, errors.New("the URL is invalid")
	}
	if body != nil {
		// S3 rejects a chunked PUT, which is what a body of unknown
		// length would be sent as.
		req.ContentLength = body.size
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return true, errors.New(withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not read into the error: S3's names the request's
		// signing details.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode >= 500, fmt.Errorf("status %d", resp.StatusCode)
	}
	if sink == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return false, nil
	}
	if err := sink(resp.Body); err != nil {
		return !errors.Is(err, errTooLarge), errors.New(withoutURL(err))
	}
	return false, nil
}

// withoutURL returns err's text without the URL a *url.Error leads with.
func withoutURL(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
