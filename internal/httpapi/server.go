// Package httpapi adapts the application service to the local HTTP API and
// serves the embedded Web UI.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/config"
	"github.com/dzaneyo/riflo/internal/web"
)

type ServerConfig struct {
	Service app.Service
	Version string
	Web     http.Handler
}

type Server struct {
	service app.Service
	version string
	web     http.Handler
	api     *http.ServeMux
}

func NewServer(cfg ServerConfig) *Server {
	version := cfg.Version
	if version == "" {
		version = app.Version
	}
	service := cfg.Service
	if service == nil {
		service = app.NewUnavailableService()
	}
	webHandler := cfg.Web
	if webHandler == nil {
		webHandler = web.Handler()
	}
	s := &Server{service: service, version: version, web: webHandler}
	s.api = http.NewServeMux()
	s.api.HandleFunc("/api/health", s.handleHealth)
	s.api.HandleFunc("/api/inspect", s.handleInspect)
	s.api.HandleFunc("/api/tasks", s.handleTasks)
	s.api.HandleFunc("/api/tasks/", s.handleTask)
	return s
}

// Handler exposes the same value as ServeHTTP for tests and embedding into a
// caller-owned http.Server.
func (s *Server) Handler() http.Handler { return s }

// HTTPServer validates the address before constructing a server. Callers can
// then use ListenAndServe or take over the listener for tests.
func (s *Server) HTTPServer(addr string) (*http.Server, error) {
	if err := config.ValidateListenAddr(addr); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !isLocalRequest(r) {
		writeError(w, app.NewError("forbidden_host", "requests must use a local Host and Origin", http.StatusForbidden))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api.ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/", "/index.html", "/app.css", "/app.js":
		s.web.ServeHTTP(w, r)
	default:
		// Serve only the explicitly embedded UI assets. Unknown non-API paths
		// remain a regular 404 instead of exposing a directory listing.
		writeError(w, app.NewError("not_found", "page not found", http.StatusNotFound))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, app.HealthResponse{
		Status:    "ok",
		Service:   "riflo",
		Version:   s.version,
		Available: true,
	})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req app.InspectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, app.Invalid(err.Error()))
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, app.Invalid("url is required"))
		return
	}
	result, err := s.service.Inspect(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := app.TaskStatus(r.URL.Query().Get("status"))
		if status != "" && !status.Valid() {
			writeError(w, app.Invalid("unknown task status"))
			return
		}
		limit := 0
		if value := r.URL.Query().Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				writeError(w, app.Invalid("limit must be a non-negative integer"))
				return
			}
			limit = parsed
		}
		tasks, err := s.service.ListTasks(r.Context(), app.TaskFilter{Status: status, Limit: limit})
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	case http.MethodPost:
		var req app.CreateTaskRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, app.Invalid(err.Error()))
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			writeError(w, app.Invalid("url is required"))
			return
		}
		task, err := s.service.CreateTask(r.Context(), req)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, task)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	parts := strings.Split(path, "/")
	id := path
	cancel := false
	if len(parts) == 2 && parts[1] == "cancel" {
		id = parts[0]
		cancel = true
	}
	if id == "" || strings.Contains(id, "/") {
		writeError(w, app.NewError("not_found", "task not found", http.StatusNotFound))
		return
	}
	switch r.Method {
	case http.MethodGet:
		if cancel {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		task, err := s.service.GetTask(r.Context(), id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
	case http.MethodPost:
		if !cancel {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		task, err := s.service.CancelTask(r.Context(), id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func isLocalRequest(r *http.Request) bool {
	if !isLocalHostPort(r.Host) {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	// A different loopback port is still a different web origin. Requiring the
	// exact Host prevents an unrelated local page from submitting state-changing
	// requests to riflo. Non-browser clients normally omit Origin and are handled
	// above.
	return strings.EqualFold(strings.TrimSuffix(u.Host, "."), strings.TrimSuffix(r.Host, "."))
}

func isLocalHostPort(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	host := value
	if h, _, err := net.SplitHostPort(value); err == nil {
		host = h
	} else if strings.Contains(value, ":") && net.ParseIP(value) == nil {
		return false
	}
	return config.IsLoopbackHost(host)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("request body is required")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func writeServiceError(w http.ResponseWriter, err error) {
	var appErr *app.Error
	if errors.As(err, &appErr) && appErr != nil {
		writeError(w, appErr)
		return
	}
	writeError(w, app.NewError("internal_error", "request failed", http.StatusInternalServerError))
}

func writeError(w http.ResponseWriter, err *app.Error) {
	status := err.Status
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code":    err.Code,
		"message": err.Message,
	}})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, app.NewError("method_not_allowed", "method not allowed", http.StatusMethodNotAllowed))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
