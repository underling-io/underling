// Package httpapi exposes the command side over HTTP.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"heinzel/internal/command"
	"heinzel/internal/eventstore"
)

const maxBodyBytes = 1 << 20

// Appender is the part of the event store the command side writes to.
type Appender interface {
	Append(stream string, expectedVersion uint64, events []eventstore.NewEvent) (version, position uint64, err error)
}

type commandRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type commandResponse struct {
	StreamID string `json:"streamId"`
	Version  uint64 `json:"version"`
	Position uint64 `json:"position"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// NewHandler returns the HTTP handler serving POST /commands.
func NewHandler(registry *command.Registry, store Appender, logger *slog.Logger) http.Handler {
	d := &dispatcher{registry: registry, store: store, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /commands", d.serveCommand)
	return mux
}

type dispatcher struct {
	registry *command.Registry
	store    Appender
	logger   *slog.Logger
}

func (d *dispatcher) serveCommand(w http.ResponseWriter, r *http.Request) {
	req, err := decodeRequest(w, r)
	if err != nil {
		d.fail(w, err)
		return
	}
	decision, err := d.registry.Decide(r.Context(), req.Type, req.Payload)
	if err != nil {
		d.fail(w, err)
		return
	}
	version, position, err := d.store.Append(decision.StreamID, decision.ExpectedVersion, decision.Events)
	if err != nil {
		d.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, commandResponse{StreamID: decision.StreamID, Version: version, Position: position})
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (commandRequest, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var req commandRequest
	if err := dec.Decode(&req); err != nil {
		return req, command.Invalid("malformed JSON: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, command.Invalid("unexpected data after JSON object")
	}
	if req.Type == "" {
		return req, command.Invalid("type must not be empty")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(req.Payload), []byte("{")) {
		return req, command.Invalid("payload must be a JSON object")
	}
	return req, nil
}

func (d *dispatcher) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, command.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, eventstore.ErrConflict):
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
	default:
		d.logger.Error("command failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
