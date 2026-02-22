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

// Get performs a GET request and returns the response body.
func (c *APIClient) Get(path string) ([]byte, error) {
	resp, err := c.client.Get(c.baseURL + path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// Post performs a POST request with a JSON body and returns the response body.
func (c *APIClient) Post(path string, jsonBody string) ([]byte, error) {
	resp, err := c.client.Post(c.baseURL+path, "application/json", strings.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// Put performs a PUT request with a JSON body and returns the response body.
func (c *APIClient) Put(path string, jsonBody string) ([]byte, error) {
	req, err := http.NewRequest("PUT", c.baseURL+path, strings.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("PUT %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// PostRaw performs a POST request with raw bytes and returns the response body.
func (c *APIClient) PostRaw(path string, data []byte) ([]byte, error) {
	resp, err := c.client.Post(c.baseURL+path, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// Delete performs a DELETE request and returns the response body.
func (c *APIClient) Delete(path string) ([]byte, error) {
	req, err := http.NewRequest("DELETE", c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("DELETE %s: %w", path, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("DELETE %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}
