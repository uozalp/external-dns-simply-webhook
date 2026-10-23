package webhook

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"sigs.k8s.io/external-dns/endpoint"
)

const (
	MediaTypeVersion = "application/external.dns.webhook+json;version=1"
	DefaultComment   = "Managed by External-DNS"
	DefaultTTL       = 3600 // 1 hour
)

// Handler handles webhook requests from ExternalDNS
type Handler struct {
	Client       *simply.Client
	Logger       *slog.Logger
	Domains      []simply.Domain
	DomainFilter []string
}

// NewHandler creates a new webhook handler
func NewHandler(client *simply.Client, logger *slog.Logger, domains []simply.Domain) *Handler {
	domainFilter := make([]string, 0, len(domains))
	for _, domain := range domains {
		domainFilter = append(domainFilter, domain.Name)
	}
	return &Handler{
		Client:       client,
		Logger:       logger,
		Domains:      domains,
		DomainFilter: domainFilter,
	}
}

func (h *Handler) Negotiate(w http.ResponseWriter, r *http.Request) {
	// Respond with the supported media type version
	response := map[string]interface{}{
		"domainFilter": h.DomainFilter,
	}

	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Error("Failed to marshal negotiation response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// GetRecords returns all current DNS records
func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {

	type endpointResponse struct {
		DNSName    string   `json:"dnsName"`
		RecordTTL  int      `json:"recordTTL"`
		RecordType string   `json:"recordType"`
		Targets    []string `json:"targets"`
	}

	var response []endpointResponse

	// Get records for each configured domain
	for _, domain := range h.Domains {

		records, err := h.Client.ListRecords(domain.Object)
		if err != nil {
			h.Logger.Error("Failed to list records for domain", "domain", domain.Name, "error", err)
			continue
		}

		h.Logger.Debug("Found records for domain", "count", len(records), "domain", domain.Name)

		// Convert Simply records to External-DNS endpoints
		for _, record := range records {
			h.Logger.Debug("Processing record", "id", record.ID, "type", record.Type, "name", record.Name, "data", record.Data)

			ep := endpointResponse{
				DNSName:    recordDNSName(record, domain),
				RecordType: record.Type,
				Targets:    []string{record.Data},
				RecordTTL:  record.TTL,
			}
			response = append(response, ep)
		}
	}

	h.Logger.Debug("Returning records", "count", len(response), "domains", len(h.DomainFilter))

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Error("Failed to marshal response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// ApplyChanges applies the desired DNS record changes
func (h *Handler) ApplyChanges(w http.ResponseWriter, r *http.Request) {

	// Define the request structure
	type Changes struct {
		Create    []*endpoint.Endpoint `json:"create"`
		UpdateOld []*endpoint.Endpoint `json:"updateOld"`
		UpdateNew []*endpoint.Endpoint `json:"updateNew"`
		Delete    []*endpoint.Endpoint `json:"delete"`
	}

	var changes Changes
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		h.Logger.Error("Failed to decode request body", "error", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.Logger.Info("Received changes", "creates", len(changes.Create), "updates", len(changes.UpdateNew), "deletes", len(changes.Delete))

	// Log the full request for debugging
	h.Logger.Debug("Full request payload", slog.Any("changes", changes))

	// Fetch all records from all domains and build a lookup map
	// Key: object:dnsName:recordType, Value: simply.Record
	recordMap := make(map[string]simply.Record)

	for _, domain := range h.Domains {
		records, err := h.Client.ListRecords(domain.Object)
		if err != nil {
			h.Logger.Error("Failed to list records for domain", "domain", domain.Name, "error", err)
			http.Error(w, fmt.Sprintf("Failed to list records: %v", err), http.StatusInternalServerError)
			return
		}

		for _, record := range records {
			// Create lookup key: object:dnsName:recordType
			key := recordKey(domain.Object, recordDNSName(record, domain), record.Type)
			recordMap[key] = record
		}
	}

	// Process creates
	for _, ep := range changes.Create {
		if err := h.createEndpoint(ep); err != nil {
			h.Logger.Error("Failed to create endpoint", "dnsName", ep.DNSName, "error", err)
			http.Error(w, fmt.Sprintf("Failed to create record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process updates - compare old and new to detect actual changes
	for i, newEp := range changes.UpdateNew {
		oldEp := changes.UpdateOld[i]

		// Check if there are actual changes
		hasChanges := false
		if oldEp.RecordType != newEp.RecordType {
			hasChanges = true
		} else if oldEp.RecordTTL != newEp.RecordTTL {
			hasChanges = true
		} else {
			// Compare targets
			for j, oldTarget := range oldEp.Targets {
				if j >= len(newEp.Targets) || oldTarget != newEp.Targets[j] {
					hasChanges = true
					break
				}
			}
		}

		if !hasChanges {
			h.Logger.Info("Skipping update - no actual changes detected", "dnsName", newEp.DNSName, "recordType", newEp.RecordType)
			continue
		}

		// Lookup record ID from map
		object, err := h.extractDomain(newEp.DNSName)
		if err != nil {
			h.Logger.Error("Failed to find domain for update", "dnsName", newEp.DNSName, "error", err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
		key := recordKey(object, newEp.DNSName, newEp.RecordType)
		existingRecord, found := recordMap[key]
		if !found {
			h.Logger.Error("Record not found in map for update", "key", key)
			http.Error(w, fmt.Sprintf("Record not found: %s", key), http.StatusInternalServerError)
			return
		}

		if err := h.updateEndpoint(newEp, existingRecord.ID); err != nil {
			h.Logger.Error("Failed to update endpoint", "dnsName", newEp.DNSName, "error", err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process deletes - lookup record ID from map
	for _, ep := range changes.Delete {
		object, err := h.extractDomain(ep.DNSName)
		if err != nil {
			h.Logger.Warn("No domain found for deletion, skipping", "dnsName", ep.DNSName, "error", err)
			continue
		}
		key := recordKey(object, ep.DNSName, ep.RecordType)
		existingRecord, found := recordMap[key]
		if !found {
			h.Logger.Warn("Record not found in map for deletion, skipping", "key", key)
			continue
		}

		if err := h.deleteEndpoint(ep, existingRecord.ID); err != nil {
			h.Logger.Error("Failed to delete endpoint", "dnsName", ep.DNSName, "error", err)
			http.Error(w, fmt.Sprintf("Failed to delete record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	h.Logger.Info("Successfully applied all changes")
	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints allows normalization or filtering of endpoints
func (h *Handler) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {

	var endpoints []*endpoint.Endpoint
	if err := json.NewDecoder(r.Body).Decode(&endpoints); err != nil {
		h.Logger.Error("Failed to decode request body", "error", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	// Normalize endpoints (e.g., ensure trailing dots, lowercase)
	for _, ep := range endpoints {
		ep.DNSName = strings.ToLower(strings.TrimSuffix(ep.DNSName, "."))
	}

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(endpoints)
	if err != nil {
		h.Logger.Error("Failed to marshal response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// Healthz returns health status
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// createEndpoint creates a new DNS record
func (h *Handler) createEndpoint(ep *endpoint.Endpoint) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	// Set default TTL if not specified
	ttl := int(ep.RecordTTL)
	if ttl == 0 {
		ttl = DefaultTTL
	}

	// Create record for each target
	for _, target := range ep.Targets {
		record := simply.Record{
			Type:    ep.RecordType,
			Name:    ep.DNSName,
			Data:    target,
			TTL:     ttl,
			Comment: DefaultComment,
		}

		h.Logger.Info("Creating Simply.com record", "domain", domain, "name", record.Name, "type", record.Type, "data", record.Data, "ttl", record.TTL)

		if err := h.Client.AddRecord(domain, record); err != nil {
			return fmt.Errorf("failed to add record: %w", err)
		}
	}

	return nil
}

// updateEndpoint updates an existing DNS record
func (h *Handler) updateEndpoint(ep *endpoint.Endpoint, recordID int) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	if len(ep.Targets) == 0 {
		return fmt.Errorf("no targets specified for update")
	}

	// Set default TTL if not specified
	ttl := int(ep.RecordTTL)
	if ttl == 0 {
		ttl = DefaultTTL
	}

	record := simply.Record{
		ID:      recordID,
		Type:    ep.RecordType,
		Name:    ep.DNSName,
		Data:    ep.Targets[0],
		TTL:     ttl,
		Comment: DefaultComment,
	}

	h.Logger.Info("Updating Simply.com record", "id", recordID, "domain", domain, "name", record.Name, "type", record.Type, "data", record.Data, "ttl", record.TTL)

	if err := h.Client.UpdateRecord(domain, record); err != nil {
		return fmt.Errorf("failed to update record: %w", err)
	}

	return nil
}

// deleteEndpoint deletes a DNS record
func (h *Handler) deleteEndpoint(ep *endpoint.Endpoint, recordID int) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	record := simply.Record{
		ID:      recordID,
		Type:    ep.RecordType,
		Name:    ep.DNSName,
		Data:    ep.Targets[0],
		TTL:     int(ep.RecordTTL),
		Comment: DefaultComment,
	}

	h.Logger.Info("Deleting Simply.com record", "id", recordID, "domain", domain, "name", record.Name, "type", record.Type)

	if err := h.Client.DeleteRecord(domain, record); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}

	return nil
}

// extractDomain returns the product object for the configured domain that
// holds dnsName. The longest match wins, so a subdomain product such as
// www.example.com is preferred over example.com.
func (h *Handler) extractDomain(dnsName string) (string, error) {
	objects := make(map[string]string, len(h.Domains)*2)
	for _, domain := range h.Domains {
		objects[strings.ToLower(domain.Object)] = domain.Object
	}
	for _, domain := range h.Domains {
		if _, exists := objects[domain.Name]; !exists {
			objects[domain.Name] = domain.Object
		}
	}

	name := strings.ToLower(strings.TrimSuffix(dnsName, "."))
	for candidate := name; candidate != ""; {
		if object, ok := objects[candidate]; ok {
			return object, nil
		}
		dot := strings.IndexByte(candidate, '.')
		if dot < 0 {
			break
		}
		candidate = candidate[dot+1:]
	}
	return "", fmt.Errorf("no managed Simply.com domain found for %s", dnsName)
}

// recordKey identifies a record by product object, DNS name and type
func recordKey(object, dnsName, recordType string) string {
	return fmt.Sprintf("%s:%s:%s", object, strings.ToLower(strings.TrimSuffix(dnsName, ".")), recordType)
}

// recordDNSName builds the full punycode DNS name of a record in domain
func recordDNSName(record simply.Record, domain simply.Domain) string {
	if record.Name == "@" || record.Name == "" {
		return domain.Name
	}
	return record.Name + "." + domain.Name
}
