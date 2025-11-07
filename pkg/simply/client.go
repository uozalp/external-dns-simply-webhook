package simply

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	BaseURL        = "https://api.simply.com"
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
	ID     int    `json:"id,omitempty"`
	Type   string `json:"type"`
	Host   string `json:"host"`
	Data   string `json:"data"`
	TTL    int    `json:"ttl"`
	Domain string `json:"domain,omitempty"`
}

// Domain represents a domain in Simply.com
type Domain struct {
	Name string `json:"name"`
}

// ListDomainsResponse represents the response from listing domains
type ListDomainsResponse struct {
	Domains []Domain `json:"domains"`
}

// ListRecordsResponse represents the response from listing records
type ListRecordsResponse struct {
	Records []Record `json:"records"`
}

// doRequest performs an HTTP request with Basic Authentication
func (c *Client) doRequest(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set Basic Authentication
	auth := base64.StdEncoding.EncodeToString([]byte(c.AccountName + ":" + c.APIKey))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
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

// ListDomains retrieves all domains from Simply.com
func (c *Client) ListDomains() ([]Domain, error) {
	respBody, err := c.doRequest("GET", "/2/domains", nil)
	if err != nil {
		return nil, err
	}

	var response ListDomainsResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return response.Domains, nil
}

// ListRecords retrieves all DNS records for a specific domain
func (c *Client) ListRecords(domain string) ([]Record, error) {
	path := fmt.Sprintf("/2/records?domain=%s", url.QueryEscape(domain))
	respBody, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var response ListRecordsResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Populate domain field in records
	for i := range response.Records {
		response.Records[i].Domain = domain
	}

	return response.Records, nil
}

// ListAllRecords retrieves all DNS records across all domains
func (c *Client) ListAllRecords() ([]Record, error) {
	domains, err := c.ListDomains()
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}

	var allRecords []Record
	for _, domain := range domains {
		records, err := c.ListRecords(domain.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to list records for domain %s: %w", domain.Name, err)
		}
		allRecords = append(allRecords, records...)
	}

	return allRecords, nil
}

// AddRecord creates a new DNS record
func (c *Client) AddRecord(record Record) error {
	_, err := c.doRequest("POST", "/2/record_add", record)
	return err
}

// UpdateRecord updates an existing DNS record
func (c *Client) UpdateRecord(record Record) error {
	_, err := c.doRequest("POST", "/2/record_update", record)
	return err
}

// DeleteRecord deletes a DNS record by ID
func (c *Client) DeleteRecord(recordID int, domain string) error {
	payload := map[string]interface{}{
		"id":     recordID,
		"domain": domain,
	}
	_, err := c.doRequest("POST", "/2/record_delete", payload)
	return err
}

// ParseFQDN splits a fully qualified domain name into host and domain parts
// e.g., "www.example.com" -> host="www", domain="example.com"
// e.g., "example.com" -> host="@", domain="example.com"
func ParseFQDN(fqdn string, domains []string) (host, domain string) {
	fqdn = strings.TrimSuffix(fqdn, ".")

	// Find the longest matching domain
	var matchedDomain string
	for _, d := range domains {
		if strings.HasSuffix(fqdn, d) && len(d) > len(matchedDomain) {
			matchedDomain = d
		}
	}

	if matchedDomain == "" {
		// No matching domain found, treat entire FQDN as host
		return fqdn, ""
	}

	if fqdn == matchedDomain {
		return "@", matchedDomain
	}

	host = strings.TrimSuffix(fqdn, "."+matchedDomain)
	return host, matchedDomain
}
