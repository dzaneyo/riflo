package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/config"
)

const defaultServerURL = "http://127.0.0.1:8787"

type localAPIClient struct {
	baseURL *url.URL
	client  *http.Client
}

func newLocalAPIClient(raw string) (*localAPIClient, error) {
	base, err := validateServerURL(raw)
	if err != nil {
		return nil, err
	}
	return &localAPIClient{
		baseURL: base,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func validateServerURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, app.Invalid("server URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return nil, app.Invalid("server URL must be an http loopback URL")
	}
	if parsed.Hostname() == "" || !config.IsLoopbackHost(parsed.Hostname()) {
		return nil, app.Invalid("server URL must use a loopback host")
	}
	if parsed.Port() != "" {
		port, portErr := strconv.Atoi(parsed.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return nil, app.Invalid("server URL has an invalid port")
		}
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, app.Invalid("server URL must not contain a query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed, nil
}

func (c *localAPIClient) ListTasks(ctx context.Context, filter app.TaskFilter) ([]app.Task, error) {
	query := url.Values{}
	if filter.Status != "" {
		query.Set("status", string(filter.Status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	var response struct {
		Tasks []app.Task `json:"tasks"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/tasks", query, nil, &response); err != nil {
		return nil, err
	}
	if response.Tasks == nil {
		response.Tasks = []app.Task{}
	}
	return response.Tasks, nil
}

func (c *localAPIClient) GetTask(ctx context.Context, id string) (app.Task, error) {
	var task app.Task
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+url.PathEscape(id), nil, nil, &task); err != nil {
		return app.Task{}, err
	}
	return task, nil
}

func (c *localAPIClient) CancelTask(ctx context.Context, id string) (app.Task, error) {
	var task app.Task
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(id)+"/cancel", nil, nil, &task); err != nil {
		return app.Task{}, err
	}
	return task, nil
}

func (c *localAPIClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader, target any) error {
	if c == nil || c.baseURL == nil || c.client == nil {
		return errors.New("riflo server client is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create server request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("cannot reach riflo serve; start it with `riflo serve`")
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeAPIError(response)
	}
	if target == nil {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return errors.New("riflo serve returned an invalid response")
	}
	return nil
}

func decodeAPIError(response *http.Response) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err == nil && payload.Error.Message != "" {
		message := safeAPIMessage(payload.Error.Message)
		if message == "" {
			message = "server request failed"
		}
		code := payload.Error.Code
		if code == "" {
			code = "server_error"
		}
		return app.NewError(code, message, response.StatusCode)
	}
	return app.NewError("server_error", fmt.Sprintf("riflo serve returned HTTP %d", response.StatusCode), response.StatusCode)
}

func safeAPIMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}
