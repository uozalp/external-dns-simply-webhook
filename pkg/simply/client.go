package simply

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	BaseURL        = "https://api.simply.com/2/"
	DefaultTimeout = 30 * time.Second
)

// Client is a Simply.com API client
type Client struct {
	AccountName string
	APIKey      string
	BaseURL     string
	HTTPClient  *http.Client
}

// NewClient creates a new Simply.com API client
func NewClient(accountName, apiKey string) *Client {
	return &Client{
		AccountName: accountName,
		APIKey:      apiKey,
		BaseURL:     BaseURL,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// Record represents a DNS record in Simply.com
type Record struct {
	ID      int    `json:"record_id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Data    string `json:"data"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment,omitempty"`
}

// makeRequest performs an HTTP request with authentication
func (c *Client) makeRequest(method, endpoint string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	url := c.BaseURL + endpoint
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add Basic Authentication
	auth := base64.StdEncoding.EncodeToString([]byte(c.AccountName + ":" + c.APIKey))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// ListDomains returns all domains managed by Simply.com
func (c *Client) ListDomains() ([]string, error) {
	resp, err := c.makeRequest("GET", "my/products", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list products: %w", err)
	}

	var result struct {
		Products []struct {
			Object string `json:"object"`
			Domain struct {
				Name    string `json:"name"`
				Managed bool   `json:"managed"`
			} `json:"domain"`
		} `json:"products"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse products response: %w", err)
	}

	var domains []string
	for _, product := range result.Products {
		// Only include managed domains
		if product.Domain.Managed {
			domains = append(domains, product.Object)
		}
	}

	return domains, nil
}

// ListRecords returns all DNS records for a domain
func (c *Client) ListRecords(domain string) ([]Record, error) {
	endpoint := fmt.Sprintf("my/products/%s/dns/records", domain)

	respBody, err := c.makeRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list records for domain %s: %w", domain, err)
	}

	var response struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Records []struct {
			RecordID int    `json:"record_id"`
			Name     string `json:"name"`
			TTL      int    `json:"ttl"`
			Data     string `json:"data"`
			Type     string `json:"type"`
		} `json:"records"`
	}

	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	var records []Record
	for _, r := range response.Records {
		records = append(records, Record{
			ID:   r.RecordID,
			Type: r.Type,
			Name: r.Name,
			Data: r.Data,
			TTL:  r.TTL,
		})
	}

	return records, nil
}

// AddRecord adds a new DNS record
func (c *Client) AddRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records", domain)

	payload := map[string]interface{}{
		"type":    record.Type,
		"name":    record.Name,
		"data":    record.Data,
		"ttl":     record.TTL,
		"comment": record.Comment,
	}

	_, err := c.makeRequest("POST", endpoint, payload)
	if err != nil {
		return fmt.Errorf("failed to add record %s %s: %w", record.Type, record.Name, err)
	}

	return nil
}

// UpdateRecord updates an existing DNS record
func (c *Client) UpdateRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/%d", domain, record.ID)

	payload := map[string]interface{}{
		"type":    record.Type,
		"name":    record.Name,
		"data":    record.Data,
		"ttl":     record.TTL,
		"comment": record.Comment,
	}

	_, err := c.makeRequest("PUT", endpoint, payload)
	if err != nil {
		return fmt.Errorf("failed to update record %s in domain %s: %w", record.Name, domain, err)
	}

	return nil
}

// DeleteRecord deletes a DNS record
func (c *Client) DeleteRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/%d", domain, record.ID)

	_, err := c.makeRequest("DELETE", endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to delete record %s from domain %s: %w", record.Name, domain, err)
	}

	return nil
}
