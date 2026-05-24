package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/drn/argus/internal/tui/views"
)

// pluginViewJSON is the wire shape for /api/plugins/views responses and the
// POST request body. created_at is RFC3339 nano per the rest of the API.
type pluginViewJSON struct {
	ID          int64  `json:"id"`
	Scope       string `json:"scope"`
	Title       string `json:"title"`
	Hotkey      string `json:"hotkey,omitempty"`
	CallbackURL string `json:"callback_url"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// pluginViewCreateReq is the POST body. The scope column is reserved for the
// post-PR-1 scope-token swap; today scope is always "" because requireMaster
// is the gate. See the TODO on each handler.
type pluginViewCreateReq struct {
	Title       string `json:"title"`
	Hotkey      string `json:"hotkey"`
	CallbackURL string `json:"callback_url"`
}

func toPluginViewJSON(v *views.View) pluginViewJSON {
	return pluginViewJSON{
		ID:          v.ID,
		Scope:       v.Scope,
		Title:       v.Title,
		Hotkey:      v.Hotkey,
		CallbackURL: v.CallbackURL,
		CreatedAt:   v.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

// handleCreatePluginView registers a new plugin-owned top-level view.
//
// TODO(post-PR-1): swap requireMaster for "master OR scope". The scope column
// on plugin_views is reserved for that follow-up; today every registration
// comes from the master token and lands with scope="".
func (s *Server) handleCreatePluginView(w http.ResponseWriter, r *http.Request) {
	if requireMaster(w, r) {
		return
	}
	var req pluginViewCreateReq
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	reg := views.New(s.db)
	v, err := reg.Register("", req.Title, req.Hotkey, req.CallbackURL)
	if err != nil {
		switch {
		case errors.Is(err, views.ErrTitleRequired),
			errors.Is(err, views.ErrCallbackURLRequired):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, views.ErrViewExists):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusCreated, toPluginViewJSON(v))
}

// handleListPluginViews returns every registered view ordered by insertion.
//
// TODO(post-PR-1): swap requireMaster for "master OR scope" and filter by the
// caller's scope. Today the master token sees every row.
func (s *Server) handleListPluginViews(w http.ResponseWriter, r *http.Request) {
	if requireMaster(w, r) {
		return
	}
	reg := views.New(s.db)
	all := reg.List()
	out := make([]pluginViewJSON, 0, len(all))
	for _, v := range all {
		out = append(out, toPluginViewJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": out})
}

// handleDeletePluginView removes the view by ID.
//
// The spec sketch called for DELETE /api/plugins/views/{scope}/{title}, but
// Go's net/http.ServeMux canonicalises `//` segments and 307-redirects, which
// makes the spec-shape unusable while every row has scope="". Using {id}
// matches the existing /api/tokens/{id} precedent and works today + after
// PR 1's scope-token swap lands.
//
// TODO(post-PR-1): swap requireMaster for "master OR scope" and reject delete
// requests whose caller's scope doesn't match the row's scope.
func (s *Server) handleDeletePluginView(w http.ResponseWriter, r *http.Request) {
	if requireMaster(w, r) {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	all, err := s.db.PluginViews()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var match *pluginViewJSON
	for _, row := range all {
		if row.ID == id {
			j := toPluginViewJSON(&views.View{
				ID: row.ID, Scope: row.Scope, Title: row.Title,
				Hotkey: row.Hotkey, CallbackURL: row.CallbackURL, CreatedAt: row.CreatedAt,
			})
			match = &j
			break
		}
	}
	if match == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "view not found"})
		return
	}
	reg := views.New(s.db)
	if err := reg.Unregister(match.Scope, match.Title); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": match.ID})
}
