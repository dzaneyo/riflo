package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dzaneyo/riflo/internal/app"
)

func TestValidateServerURLOnlyAllowsHTTPLoopback(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8787",
		"http://localhost:8787/",
		"http://[::1]:8787",
	}
	for _, value := range valid {
		if _, err := validateServerURL(value); err != nil {
			t.Errorf("validateServerURL(%q): %v", value, err)
		}
	}
	invalid := []string{
		"",
		"https://127.0.0.1:8787",
		"http://example.test:8787",
		"http://127.0.0.1:8787?token=secret",
		"http://127.0.0.1:8787/#fragment",
		"http://user:pass@127.0.0.1:8787",
	}
	for _, value := range invalid {
		if _, err := validateServerURL(value); err == nil {
			t.Errorf("validateServerURL(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLocalAPIClientRequestsTasksTaskAndCancel(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []app.Task{{ID: "task-one", Status: app.TaskRunning}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks/task-one":
			_ = json.NewEncoder(w).Encode(app.Task{ID: "task-one", Status: app.TaskRunning})
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/task-one/cancel":
			_ = json.NewEncoder(w).Encode(app.Task{ID: "task-one", Status: app.TaskCanceled})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newLocalAPIClient(server.URL)
	if err != nil {
		t.Fatalf("newLocalAPIClient: %v", err)
	}
	items, err := client.ListTasks(context.Background(), app.TaskFilter{Status: app.TaskRunning})
	if err != nil || len(items) != 1 || items[0].ID != "task-one" {
		t.Fatalf("ListTasks = %#v, %v", items, err)
	}
	task, err := client.GetTask(context.Background(), "task-one")
	if err != nil || task.Status != app.TaskRunning {
		t.Fatalf("GetTask = %#v, %v", task, err)
	}
	task, err = client.CancelTask(context.Background(), "task-one")
	if err != nil || task.Status != app.TaskCanceled {
		t.Fatalf("CancelTask = %#v, %v", task, err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{"GET /api/tasks?status=running", "GET /api/tasks/task-one", "POST /api/tasks/task-one/cancel"} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests = %q, missing %q", joined, want)
		}
	}
}

func TestLocalAPIClientUnavailableMessage(t *testing.T) {
	client, err := newLocalAPIClient("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListTasks(context.Background(), app.TaskFilter{})
	if err == nil || !strings.Contains(err.Error(), "start it with `riflo serve`") {
		t.Fatalf("unavailable error = %v", err)
	}
}
