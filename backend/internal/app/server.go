package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type Server struct {
	cfg     Config
	store   *Store
	service *Service
}

func NewServer(cfg Config, store *Store) *Server {
	return &Server{cfg: cfg, store: store, service: NewService(cfg, store)}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("POST /api/threads/", s.handleThreads)
	mux.HandleFunc("POST /api/jobs/", s.handleJobs)
	mux.HandleFunc("PATCH /api/replies/", s.handleReplies)
	mux.HandleFunc("POST /api/replies/", s.handleReplies)
	return cors(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"checks": s.service.Health(r.Context()), "checked_at": utcNow()})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.service.DashboardSnapshot()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.SyncSlack()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	id, suffix, ok := parseActionPath(r.URL.Path, "/api/threads/")
	if !ok || suffix != "analyze" {
		http.NotFound(w, r)
		return
	}
	result, err := s.service.QueueAnalysis(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	id, suffix, ok := parseActionPath(r.URL.Path, "/api/jobs/")
	if !ok || suffix != "run" {
		http.NotFound(w, r)
		return
	}
	result, err := s.service.QueueJob(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleReplies(w http.ResponseWriter, r *http.Request) {
	id, suffix, ok := parseActionPath(r.URL.Path, "/api/replies/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPatch && suffix == "":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		reply, err := s.service.UpdateReply(id, body.Text)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, reply)
	case r.Method == http.MethodPost && suffix == "send":
		result, err := s.service.SendReply(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodPost && suffix == "archive":
		result, err := s.service.ArchiveReply(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		http.NotFound(w, r)
	}
}

func parseActionPath(path, prefix string) (int64, string, bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path {
		return 0, "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
	}
	return id, suffix, true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
