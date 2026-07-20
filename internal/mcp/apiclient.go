package mcp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIClient is a simple HTTP client for calling the OpenPoet API.
type APIClient struct {
	baseURL string
	token   string // opst1_ session bearer attached to every request when set
	client  *http.Client
}

// NewAPIClient creates a new API client with the given base URL.
func NewAPIClient(baseURL string) *APIClient {
	return &APIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewAPIClientWithToken creates an API client that carries a per-session bearer
// on every request, so the OpenPoet REST layer resolves the true actor.
func NewAPIClientWithToken(baseURL, token string) *APIClient {
	c := NewAPIClient(baseURL)
	c.token = strings.TrimSpace(token)
	return c
}

// do builds and executes a request, attaching the session bearer when present.
func (c *APIClient) do(method, path, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// Get performs a GET request and returns the response body.
func (c *APIClient) Get(path string) ([]byte, error) {
	return c.do("GET", path, "", nil)
}

// Post performs a POST request with a JSON body and returns the response body.
func (c *APIClient) Post(path string, jsonBody string) ([]byte, error) {
	return c.do("POST", path, "application/json", strings.NewReader(jsonBody))
}

// Put performs a PUT request with a JSON body and returns the response body.
func (c *APIClient) Put(path string, jsonBody string) ([]byte, error) {
	return c.do("PUT", path, "application/json", strings.NewReader(jsonBody))
}

// PostRaw performs a POST request with raw bytes and returns the response body.
func (c *APIClient) PostRaw(path string, data []byte) ([]byte, error) {
	return c.do("POST", path, "application/octet-stream", bytes.NewReader(data))
}

// Delete performs a DELETE request and returns the response body.
func (c *APIClient) Delete(path string) ([]byte, error) {
	return c.do("DELETE", path, "", nil)
}
