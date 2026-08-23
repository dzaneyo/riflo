package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dzaneyo/riflo/internal/app"
)

type testService struct {
	canceled string
}

func (s *testService) Inspect(context.Context, app.InspectRequest) (app.MediaInfo, error) {
	return app.MediaInfo{}, nil
}

func (s *testService) CreateTask(context.Context, app.CreateTaskRequest) (app.Task, error) {
	return app.Task{ID: "new", Status: app.TaskQueued}, nil
}

func (s *testService) ListTasks(context.Context, app.TaskFilter) ([]app.Task, error) {
	return []app.Task{}, nil
}

func (s *testService) GetTask(context.Context, string) (app.Task, error) {
	return app.Task{ID: "one", Status: app.TaskRunning}, nil
}

func (s *testService) CancelTask(_ context.Context, id string) (app.Task, error) {
	s.canceled = id
	return app.Task{ID: id, Status: app.TaskCanceled}, nil
}

func localRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:8787"
	return r
}

func TestHealth(t *testing.T) {
	server := NewServer(ServerConfig{Version: "test"})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, localRequest(http.MethodGet, "http://127.0.0.1:8787/api/health"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("health content type = %q", got)
	}
	if body := recorder.Body.String(); body == "" || !containsAll(body, `"status":"ok"`, `"version":"test"`) {
		t.Fatalf("health body = %q", body)
	}
}

func TestRejectsNonLocalHostAndOrigin(t *testing.T) {
	server := NewServer(ServerConfig{})
	tests := []struct {
		name   string
		host   string
		origin string
	}{
		{name: "host", host: "example.com:8787"},
		{name: "origin", host: "127.0.0.1:8787", origin: "https://example.com"},
		{name: "different loopback port", host: "127.0.0.1:8787", origin: "http://127.0.0.1:9999"},
		{name: "different loopback name", host: "127.0.0.1:8787", origin: "http://localhost:8787"},
		{name: "null origin", host: "127.0.0.1:8787", origin: "null"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/health", nil)
			req.Host = test.host
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
			}
		})
	}
}

func TestAcceptsSameOrigin(t *testing.T) {
	server := NewServer(ServerConfig{})
	recorder := httptest.NewRecorder()
	req := localRequest(http.MethodGet, "http://127.0.0.1:8787/api/health")
	req.Header.Set("Origin", "http://127.0.0.1:8787")
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing frame protection header")
	}
}

func TestCancelRoute(t *testing.T) {
	service := &testService{}
	server := NewServer(ServerConfig{Service: service})
	recorder := httptest.NewRecorder()
	req := localRequest(http.MethodPost, "http://127.0.0.1:8787/api/tasks/task-123/cancel")
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if service.canceled != "task-123" {
		t.Fatalf("canceled id = %q, want task-123", service.canceled)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		found := false
		for i := 0; i+len(part) <= len(value); i++ {
			if value[i:i+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
