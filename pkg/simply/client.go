package simply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	BaseURL        = "https://api.simply.com/2/"
	DefaultTimeout = 30 * time.Second

	maxResponseBytes = 10 << 20
	maxErrorBodyLen  = 512
	maxAttempts      = 3
	maxRetryWait     = 10 * time.Second
)

// Client is a Simply.com API client
type Client struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	// RetryDelay is the pause before the first retry; it doubles on each further retry.
	RetryDelay time.Duration

	sleep func(time.Duration)
}

// apiError is a non-2xx answer from Simply.com. RetryAfter is the wait the
// server asked for, or zero.
type apiError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("API request failed with status %d: %s", e.Status, e.Body)
}

// parseRetryAfter reads a Retry-After header given in seconds or as an HTTP date.
func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// NewClient creates a new Simply.com API client
func NewClient(apiKey string) *Client {
	return &Client{
		APIKey:  apiKey,
		BaseURL: BaseURL,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		RetryDelay: 500 * time.Millisecond,
		sleep:      time.Sleep,
	}
}

// Record represents a DNS record in Simply.com. Priority is set for MX and SRV
// records and is nil for all other types.
type Record struct {
	ID       int    `json:"record_id,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// Domain is a DNS-managed product on the account. Object is the product
// handle used in API paths, Name is the punycode (ASCII) domain name.
type Domain struct {
	Object string
	Name   string
}

// DomainIndex maps each lowercase object and name to its domain; the object wins on a clash.
func DomainIndex(domains []Domain) map[string]Domain {
	index := make(map[string]Domain, len(domains)*2)
	for _, d := range domains {
		if _, ok := index[d.Name]; !ok {
			index[d.Name] = d
		}
	}
	for _, d := range domains {
		index[strings.ToLower(d.Object)] = d
	}
	return index
}

// FilterDomains resolves requested names (Unicode or punycode) to domains, once each.
// unknown lists the names that match no domain.
func FilterDomains(all []Domain, requested []string) (found []Domain, unknown []string) {
	index := DomainIndex(all)
	seen := make(map[string]bool)
	for _, name := range requested {
		name = strings.TrimSpace(name)
		d, ok := index[strings.ToLower(name)]
		if !ok {
			unknown = append(unknown, name)
		} else if !seen[d.Object] {
			seen[d.Object] = true
			found = append(found, d)
		}
	}
	return found, unknown
}

// makeRequest performs an HTTP request with authentication. Requests that are
// safe to repeat (everything except POST) are retried on 429 and 5xx answers
// and on network errors. A 429 wait follows the Retry-After header. A retried
// DELETE that gets 404 counts as done, because the lost first try may have worked.
func (c *Client) makeRequest(method, endpoint string, body interface{}) ([]byte, error) {
	var jsonBody []byte
	if body != nil {
		var err error
		jsonBody, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	attempts := 1
	if method != http.MethodPost {
		attempts = maxAttempts
	}

	delay := c.RetryDelay
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		respBody, err := c.doRequest(method, endpoint, jsonBody)
		if err == nil {
			return respBody, nil
		}

		var apiErr *apiError
		isAPIError := errors.As(err, &apiErr)
		if isAPIError && apiErr.Status == http.StatusNotFound && method == http.MethodDelete && attempt > 1 {
			return nil, nil
		}
		lastErr = err
		retry := !isAPIError || apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
		if !retry || attempt == attempts {
			break
		}

		wait := delay
		if isAPIError {
			wait = max(wait, apiErr.RetryAfter)
		}
		c.sleep(min(wait, maxRetryWait))
		delay *= 2
	}
	return nil, lastErr
}

// doRequest sends one request. A non-2xx answer is returned as an *apiError.
func (c *Client) doRequest(method, endpoint string, jsonBody []byte) ([]byte, error) {
	var reqBody io.Reader
	if jsonBody != nil {
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, c.BaseURL+endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respBody)
		if len(msg) > maxErrorBodyLen {
			msg = msg[:maxErrorBodyLen] + "..."
		}
		return nil, &apiError{
			Status:     resp.StatusCode,
			Body:       msg,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	return respBody, nil
}

// ListDomains returns all domains managed by Simply.com
func (c *Client) ListDomains() ([]Domain, error) {
	resp, err := c.makeRequest("GET", "my/products/", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list products: %w", err)
	}

	var result struct {
		Products []struct {
			Object string `json:"object"`
			Domain struct {
				Name    string `json:"name"`
				NameIDN string `json:"name_idn"`
				Managed bool   `json:"managed"`
			} `json:"domain"`
		} `json:"products"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse products response: %w", err)
	}

	var domains []Domain
	for _, product := range result.Products {
		// Only include managed domains
		if !product.Domain.Managed {
			continue
		}
		name := product.Domain.NameIDN
		if name == "" {
			name = product.Object
		}
		domains = append(domains, Domain{
			Object: product.Object,
			Name:   strings.ToLower(name),
		})
	}

	return domains, nil
}

// ListRecords returns all DNS records for a domain
func (c *Client) ListRecords(domain string) ([]Record, error) {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/", url.PathEscape(domain))

	respBody, err := c.makeRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list records for domain %s: %w", domain, err)
	}

	var response struct {
		Records []Record `json:"records"`
	}

	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return response.Records, nil
}

// body is the JSON payload Simply.com expects when writing a record. Updates
// replace the whole record, so the priority must be sent again.
func (r Record) body() map[string]interface{} {
	body := map[string]interface{}{
		"type":    r.Type,
		"name":    r.Name,
		"data":    r.Data,
		"ttl":     r.TTL,
		"comment": r.Comment,
	}
	if r.Priority != nil {
		body["priority"] = *r.Priority
	}
	return body
}

// AddRecord adds a new DNS record
func (c *Client) AddRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/", url.PathEscape(domain))

	_, err := c.makeRequest("POST", endpoint, record.body())
	if err != nil {
		return fmt.Errorf("failed to add record %s %s: %w", record.Type, record.Name, err)
	}

	return nil
}

// UpdateRecord updates an existing DNS record
func (c *Client) UpdateRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/%d/", url.PathEscape(domain), record.ID)

	_, err := c.makeRequest("PUT", endpoint, record.body())
	if err != nil {
		return fmt.Errorf("failed to update record %s in domain %s: %w", record.Name, domain, err)
	}

	return nil
}

// DeleteRecord deletes a DNS record
func (c *Client) DeleteRecord(domain string, record Record) error {
	endpoint := fmt.Sprintf("my/products/%s/dns/records/%d/", url.PathEscape(domain), record.ID)

	_, err := c.makeRequest("DELETE", endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to delete record %s from domain %s: %w", record.Name, domain, err)
	}

	return nil
}
