package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/drn/argus/internal/db"
)

// taskMetaMaxBodyBytes caps a single PUT /api/tasks/{id}/meta request body.
// The PR 3 plan deliberately leaves task_meta storage unbounded (the table is
// a free-form sidecar — see the "task_meta size cap" open question), but the
// HTTP layer still needs a ceiling so a runaway client can't pin the daemon
// in JSON decoding. 1 MiB is generous (≈ 16 KiB per entry × 64 entries) and
// roughly an order of magnitude above any realistic batch.
const taskMetaMaxBodyBytes = 1 * 1024 * 1024

// metaEntryJSON is the wire shape returned by handleGetMeta. UpdatedAt is
// serialized via time.Time's RFC3339 default so the SPA can render it
// directly; clients only need the field as an opaque sortable string.
type metaEntryJSON struct {
	Namespace string    `json:"namespace"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// handleGetMeta returns the sidecar metadata for the path-bound task. When
// ?namespace=<ns> is set, results are scoped to that namespace; otherwise
// every namespace is returned.
//
// Reads are open to any authenticated request (master OR device) — symmetric
// with handleListInbox and the other per-task read endpoints. Writes go
// through handlePutMeta and require master.
func (s *Server) handleGetMeta(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.Get(id); err != nil {
		if errors.Is(err, db.ErrTaskNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	namespace := r.URL.Query().Get("namespace")
	rows, err := s.db.ListMeta(id, namespace)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	entries := make([]metaEntryJSON, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, metaEntryJSON{
			Namespace: e.Namespace,
			Key:       e.Key,
			Value:     e.Value,
			UpdatedAt: e.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// metaPutReq is the union shape accepted by PUT /api/tasks/{id}/meta. Exactly
// one of (Key+Value) or Entries must be set. Namespace is always required.
//
// When PR 1 (scope-tagged tokens) lands, the namespace will be auto-derived
// from the auth scope and rejected if it doesn't match. For now writes are
// master-only and the body's namespace is taken as-is.
type metaPutReq struct {
	Namespace string            `json:"namespace"`
	Key       string            `json:"key"`
	Value     string            `json:"value"`
	Entries   map[string]string `json:"entries"`
}

// handlePutMeta upserts one row or a batch of rows under the path-bound
// task's metadata. requireMaster — sidecar writes mutate cross-plugin state
// and the device-token tier is read-only by default. See metaPutReq for the
// accepted body shapes.
func (s *Server) handlePutMeta(w http.ResponseWriter, r *http.Request) {
	if requireMaster(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.db.Get(id); err != nil {
		if errors.Is(err, db.ErrTaskNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, taskMetaMaxBodyBytes)
	var req metaPutReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Namespace == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace is required"})
		return
	}

	// Disambiguate single vs. batch. A request that sets both shapes is
	// rejected — silently picking one would mask client bugs.
	singleSet := req.Key != "" || req.Value != ""
	batchSet := len(req.Entries) > 0
	switch {
	case !singleSet && !batchSet:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "either {key,value} or {entries} is required"})
		return
	case singleSet && batchSet:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "set either {key,value} or {entries}, not both"})
		return
	case singleSet:
		if req.Key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
			return
		}
		// SetMeta's only validation (ErrMetaInvalidKey on empty task_id /
		// namespace / key) is unreachable here — the handler-level checks
		// above already enforce all three are non-empty. So any error
		// returned is a SQL-tier failure and maps to 500.
		if err := s.db.SetMeta(id, req.Namespace, req.Key, req.Value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"written": 1})
	case batchSet:
		if err := s.db.SetMetaBatch(id, req.Namespace, req.Entries); err != nil {
			if errors.Is(err, db.ErrMetaInvalidKey) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"written": len(req.Entries)})
	}
}
