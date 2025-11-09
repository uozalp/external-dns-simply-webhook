package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"sigs.k8s.io/external-dns/endpoint"
)

const (
	MediaTypeVersion     = "application/external.dns.webhook+json;version=1"
	ManagedByExternalDNS = "Managed by External-DNS"
)

// Handler handles webhook requests from ExternalDNS
type Handler struct {
	Client       *simply.Client
	Logger       *logrus.Logger
	DomainFilter []string
}

// NewHandler creates a new webhook handler
func NewHandler(client *simply.Client, logger *logrus.Logger, domainFilter []string) *Handler {
	return &Handler{
		Client:       client,
		Logger:       logger,
		DomainFilter: domainFilter,
	}
}

func (h *Handler) Negotiate(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	h.Logger.Infof("GET / called with Accept: %s", accept)

	// Respond with the supported media type version
	response := map[string]interface{}{
		"domainFilter": h.DomainFilter,
	}

	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Errorf("Failed to marshal negotiation response: %v", err)
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
	h.Logger.Info("GET /records called")

	type providerSpecific struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}

	type endpointResponse struct {
		DNSName          string             `json:"dnsName"`
		RecordTTL        int                `json:"recordTTL"`
		RecordType       string             `json:"recordType"`
		Targets          []string           `json:"targets"`
		ProviderSpecific []providerSpecific `json:"providerSpecific,omitempty"`
	}

	var response []endpointResponse

	// Get records for each configured domain
	for _, domain := range h.DomainFilter {
		h.Logger.Infof("Fetching records for domain: %s", domain)

		records, err := h.Client.ListRecords(domain)
		if err != nil {
			h.Logger.Errorf("Failed to list records for domain %s: %v", domain, err)
			continue
		}

		h.Logger.Infof("Found %d records for domain %s", len(records), domain)

		// Convert Simply records to External-DNS endpoints
		for _, record := range records {
			h.Logger.Debugf("Processing record: ID=%d, Type=%s, Name=%s, Data=%s",
				record.ID, record.Type, record.Name, record.Data)

			// Build full DNS name
			var dnsName string
			if record.Name == "@" || record.Name == "" {
				dnsName = domain
			} else {
				dnsName = record.Name + "." + domain
			}

			ep := endpointResponse{
				DNSName:    dnsName,
				RecordType: record.Type,
				Targets:    []string{record.Data},
				RecordTTL:  record.TTL,
				ProviderSpecific: []providerSpecific{
					{
						Name:  "simply-record-id",
						Value: fmt.Sprintf("%d", record.ID),
					},
				},
			}
			response = append(response, ep)
		}
	}

	h.Logger.Infof("Returning %d records across %d domains", len(response), len(h.DomainFilter))

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Errorf("Failed to marshal response: %v", err)
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
	h.Logger.Info("POST /records called")

	// Define the request structure
	type Changes struct {
		Create    []*endpoint.Endpoint `json:"create"`
		UpdateOld []*endpoint.Endpoint `json:"updateOld"`
		UpdateNew []*endpoint.Endpoint `json:"updateNew"`
		Delete    []*endpoint.Endpoint `json:"delete"`
	}

	var changes Changes
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.Logger.Infof("Received changes: %d creates, %d updates, %d deletes",
		len(changes.Create), len(changes.UpdateNew), len(changes.Delete))

	// Log the full request for debugging
	reqJSON, _ := json.MarshalIndent(changes, "", "  ")
	h.Logger.Debugf("Full request payload:\n%s", string(reqJSON))

	// Process creates
	for _, ep := range changes.Create {
		h.Logger.Infof("Creating record: %s %s -> %v (TTL: %d)",
			ep.DNSName, ep.RecordType, ep.Targets, ep.RecordTTL)
		if err := h.createEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to create endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to create record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process updates (we only need UpdateNew since we have the record ID)
	for _, ep := range changes.UpdateNew {
		h.Logger.Infof("Updating record: %s %s -> %v (TTL: %d)",
			ep.DNSName, ep.RecordType, ep.Targets, ep.RecordTTL)
		if err := h.updateEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to update endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process deletes
	for _, ep := range changes.Delete {
		h.Logger.Infof("Deleting record: %s %s", ep.DNSName, ep.RecordType)
		if err := h.deleteEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to delete endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to delete record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	h.Logger.Info("Successfully applied all changes")
	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints allows normalization or filtering of endpoints
func (h *Handler) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("POST /adjustendpoints called")

	var endpoints []*endpoint.Endpoint
	if err := json.NewDecoder(r.Body).Decode(&endpoints); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
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
		h.Logger.Errorf("Failed to marshal response: %v", err)
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

	// Create record for each target
	for _, target := range ep.Targets {
		record := simply.Record{
			Type:    ep.RecordType,
			Name:    ep.DNSName,
			Data:    target,
			TTL:     int(ep.RecordTTL),
			Comment: ManagedByExternalDNS,
		}

		h.Logger.Infof("Creating Simply.com record: domain=%s, name=%s, type=%s, data=%s, ttl=%d",
			domain, record.Name, record.Type, record.Data, record.TTL)

		if err := h.Client.AddRecord(domain, record); err != nil {
			return fmt.Errorf("failed to add record: %w", err)
		}
	}

	return nil
}

// updateEndpoint updates an existing DNS record
func (h *Handler) updateEndpoint(ep *endpoint.Endpoint) error {
	// Extract Simply.com record ID from providerSpecific
	recordID, err := h.getRecordID(ep)
	if err != nil {
		return fmt.Errorf("failed to get record ID: %w", err)
	}

	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	// Update record (assuming single target for updates)
	if len(ep.Targets) == 0 {
		return fmt.Errorf("no targets specified for update")
	}

	record := simply.Record{
		ID:      recordID,
		Type:    ep.RecordType,
		Name:    ep.DNSName,
		Data:    ep.Targets[0],
		TTL:     int(ep.RecordTTL),
		Comment: ManagedByExternalDNS,
	}

	h.Logger.Infof("Updating Simply.com record: id=%d, domain=%s, name=%s, type=%s, data=%s, ttl=%d",
		recordID, domain, record.Name, record.Type, record.Data, record.TTL)

	if err := h.Client.UpdateRecord(domain, record); err != nil {
		return fmt.Errorf("failed to update record: %w", err)
	}

	return nil
}

// deleteEndpoint deletes a DNS record
func (h *Handler) deleteEndpoint(ep *endpoint.Endpoint) error {
	// Extract Simply.com record ID from providerSpecific
	recordID, err := h.getRecordID(ep)
	if err != nil {
		return fmt.Errorf("failed to get record ID: %w", err)
	}

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
		Comment: ManagedByExternalDNS,
	}

	h.Logger.Infof("Deleting Simply.com record id=%d, domain=%s, name=%s, type=%s, data=%s, ttl=%d",
		recordID, domain, record.Name, record.Type, record.Data, record.TTL)

	if err := h.Client.DeleteRecord(domain, record); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}

	return nil
}

// getRecordID extracts the Simply.com record ID from providerSpecific field
func (h *Handler) getRecordID(ep *endpoint.Endpoint) (int, error) {
	for _, ps := range ep.ProviderSpecific {
		if ps.Name == "simply-record-id" {
			var recordID int
			_, err := fmt.Sscanf(ps.Value, "%d", &recordID)
			if err != nil {
				return 0, fmt.Errorf("invalid record ID: %s", ps.Value)
			}
			return recordID, nil
		}
	}
	return 0, fmt.Errorf("simply-record-id not found in providerSpecific")
}

// extractDomain extracts the base domain from a DNS name
func (h *Handler) extractDomain(dnsName string) (string, error) {
	parts := strings.Split(dnsName, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid DNS name: %s", dnsName)
	}
	return strings.Join(parts[len(parts)-2:], "."), nil
}
