// Package runnertest holds what tests of the runner exchange share: an
// HTTP file server that stands in for S3 and its presigned URLs.
package runnertest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"

	"underling/internal/slices/investigatesentryissue/contract"
)

// Signature is the query every URL of a Files server carries, as a
// presigned URL carries its signature. A request without it gets 403, and
// no log line, error or result may show it.
const Signature = "X-Amz-Signature=s3cr3t-signature"

// Files is an in-memory HTTP file server: it stores PUTs and serves GETs.
// Like S3 it answers 411 to a PUT without a Content-Length.
type Files struct {
	Server *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	status  map[string][]int
}

// NewFiles starts a file server; Close stops it.
func NewFiles() *Files {
	f := &Files{objects: map[string][]byte{}, status: map[string][]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the server.
func (f *Files) Close() { f.Server.Close() }

func (f *Files) serve(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path[1:]
	if r.URL.RawQuery != Signature {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	if status := f.nextStatus(r.Method + " " + key); status != 0 {
		http.Error(w, "refused", status)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		data, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		if r.ContentLength < 0 {
			http.Error(w, "MissingContentLength", http.StatusLengthRequired)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objects[key] = data
		f.puts = append(f.puts, key)
		f.mu.Unlock()
	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

func (f *Files) nextStatus(request string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	queue := f.status[request]
	if len(queue) == 0 {
		return 0
	}
	if len(queue) > 1 {
		f.status[request] = queue[1:]
	}
	return queue[0]
}

// Answer makes the server answer the next requests for "<METHOD> <key>"
// with statuses in turn instead of serving them, repeating the last; a
// final 0 serves again.
func (f *Files) Answer(method, key string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[method+" "+key] = statuses
}

// URL returns key's URL, signature included.
func (f *Files) URL(key string) string { return f.Server.URL + "/" + key + "?" + Signature }

// URLs returns the four URLs of run runID's files.
func (f *Files) URLs(runID string) contract.URLs {
	return contract.URLs{
		InputBundle:  f.URL(contract.ObjectKey(runID, contract.InputBundle)),
		Prompt:       f.URL(contract.ObjectKey(runID, contract.InputPrompt)),
		OutputBundle: f.URL(contract.ObjectKey(runID, contract.OutputBundle)),
		Result:       f.URL(contract.ObjectKey(runID, contract.OutputResult)),
	}
}

// Put stores data under key, as underling's upload would.
func (f *Files) Put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

// Get returns what is stored under key.
func (f *Files) Get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return data, ok
}

// Puts returns the keys the server stored PUTs under, in order.
func (f *Files) Puts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.puts)
}
