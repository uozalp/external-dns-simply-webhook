package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
)

const (
	MediaTypeVersion = "application/external.dns.webhook+json;version=1"
	DefaultComment   = "Managed by External-DNS"
	DefaultTTL       = 3600 // 1 hour

	maxBodyBytes     = 10 << 20
	maxParallelLists = 4
)

// endpoint is the part of an ExternalDNS endpoint this webhook reads and writes.
type endpoint struct {
	DNSName    string   `json:"dnsName"`
	Targets    []string `json:"targets"`
	RecordType string   `json:"recordType"`
	RecordTTL  int      `json:"recordTTL"`
}

// recordIndex holds the existing Simply records by recordKey.
type recordIndex map[string][]simply.Record

// domainSet is an immutable view of the managed domains.
type domainSet struct {
	domains []simply.Domain
	filter  []string
	index   map[string]simply.Domain
}

// Handler handles webhook requests from ExternalDNS
type Handler struct {
	Client *simply.Client
	Logger *slog.Logger

	domains atomic.Pointer[domainSet]
	// applyMu keeps two ApplyChanges calls from both seeing "record missing" and both creating it.
	applyMu sync.Mutex
}

// NewHandler creates a new webhook handler
func NewHandler(client *simply.Client, logger *slog.Logger, domains []simply.Domain) *Handler {
	h := &Handler{Client: client, Logger: logger}
	h.SetDomains(domains)
	return h
}

// SetDomains replaces the managed domains. It is safe to call while requests are served.
func (h *Handler) SetDomains(domains []simply.Domain) {
	filter := make([]string, 0, len(domains))
	for _, domain := range domains {
		filter = append(filter, domain.Name)
	}
	h.domains.Store(&domainSet{domains: domains, filter: filter, index: simply.DomainIndex(domains)})
}

// writeJSON marshals first so the response has a Content-Length instead of chunked encoding.
func (h *Handler) writeJSON(w http.ResponseWriter, v any) {
	jsonData, err := json.Marshal(v)
	if err != nil {
		h.Logger.Error("Failed to marshal response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", strconv.Itoa(len(jsonData)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonData); err != nil {
		h.Logger.Error("Failed to write response", "error", err)
	}
}

// Negotiate tells ExternalDNS which domains this webhook manages
func (h *Handler) Negotiate(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, map[string]any{"domainFilter": h.domains.Load().filter})
}

// GetRecords returns all current DNS records
func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {
	set := h.domains.Load()
	lists, err := h.listRecords(set.domains)
	if err != nil {
		// A partial list would make ExternalDNS re-create the records that are missing from it.
		http.Error(w, fmt.Sprintf("Failed to list records: %v", err), http.StatusInternalServerError)
		return
	}

	response := []endpoint{}
	for i, domain := range set.domains {
		h.Logger.Debug("Found records for domain", "count", len(lists[i]), "domain", domain.Name)
		response = append(response, groupRecords(lists[i], domain)...)
	}

	h.Logger.Debug("Returning records", "count", len(response), "domains", len(set.domains))
	h.writeJSON(w, response)
}

// ApplyChanges applies the desired DNS record changes
func (h *Handler) ApplyChanges(w http.ResponseWriter, r *http.Request) {
	var changes struct {
		Create    []endpoint `json:"create"`
		UpdateNew []endpoint `json:"updateNew"`
		Delete    []endpoint `json:"delete"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&changes); err != nil {
		h.Logger.Error("Failed to decode request body", "error", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.applyMu.Lock()
	defer h.applyMu.Unlock()

	h.Logger.Info("Received changes", "creates", len(changes.Create), "updates", len(changes.UpdateNew), "deletes", len(changes.Delete))
	h.Logger.Debug("Full request payload", slog.Any("changes", changes))

	existing, err := h.loadRecords(changes.Create, changes.UpdateNew, changes.Delete)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list records: %v", err), http.StatusInternalServerError)
		return
	}

	steps := []struct {
		action    string
		endpoints []endpoint
		apply     func(endpoint, recordIndex) error
	}{
		{"create", changes.Create, h.createEndpoint},
		{"update", changes.UpdateNew, h.updateEndpoint},
		{"delete", changes.Delete, h.deleteEndpoint},
	}
	for _, step := range steps {
		for _, ep := range step.endpoints {
			if err := step.apply(ep, existing); err != nil {
				h.Logger.Error("Failed to apply endpoint", "action", step.action, "dnsName", ep.DNSName, "error", err)
				http.Error(w, fmt.Sprintf("Failed to %s record: %v", step.action, err), http.StatusInternalServerError)
				return
			}
		}
	}

	h.Logger.Info("Successfully applied all changes")
	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints normalizes endpoint names. Every other field is passed through untouched.
func (h *Handler) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	var endpoints []map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&endpoints); err != nil {
		h.Logger.Error("Failed to decode request body", "error", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	for _, ep := range endpoints {
		if name, ok := ep["dnsName"].(string); ok {
			ep["dnsName"] = strings.ToLower(strings.TrimSuffix(name, "."))
		}
	}

	h.writeJSON(w, endpoints)
}

// Healthz returns health status
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// listRecords fetches the records of several domains in parallel. The result
// has the same order as domains; any failure fails the whole call.
func (h *Handler) listRecords(domains []simply.Domain) ([][]simply.Record, error) {
	results := make([][]simply.Record, len(domains))
	errs := make([]error, len(domains))
	slots := make(chan struct{}, maxParallelLists)

	var wg sync.WaitGroup
	for i, domain := range domains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			results[i], errs[i] = h.Client.ListRecords(domain.Object)
			if errs[i] != nil {
				h.Logger.Error("Failed to list records for domain", "domain", domain.Name, "error", errs[i])
			}
		}()
	}
	wg.Wait()

	return results, errors.Join(errs...)
}

// loadRecords fetches the records of the domains the endpoints belong to.
// Names outside the managed domains are left to the apply step to report.
func (h *Handler) loadRecords(groups ...[]endpoint) (recordIndex, error) {
	var domains []simply.Domain
	for _, group := range groups {
		for _, ep := range group {
			domain, err := h.findDomain(ep.DNSName)
			if err == nil && !slices.Contains(domains, domain) {
				domains = append(domains, domain)
			}
		}
	}

	lists, err := h.listRecords(domains)
	if err != nil {
		return nil, err
	}

	existing := make(recordIndex)
	for i, domain := range domains {
		for _, record := range lists[i] {
			key := recordKey(domain.Object, recordDNSName(record, domain), record.Type)
			existing[key] = append(existing[key], record)
		}
	}
	return existing, nil
}

// createEndpoint adds one record per target that does not exist yet
func (h *Handler) createEndpoint(ep endpoint, existing recordIndex) error {
	domain, err := h.findDomain(ep.DNSName)
	if err != nil {
		return err
	}

	have := existing[recordKey(domain.Object, ep.DNSName, ep.RecordType)]
	var added []string
	for _, target := range ep.Targets {
		if slices.Contains(added, target) {
			continue
		}
		added = append(added, target)
		if hasData(have, target) {
			h.Logger.Info("Skipping create - record already exists", "dnsName", ep.DNSName, "recordType", ep.RecordType, "data", target)
			continue
		}
		if err := h.addRecord(domain, ep, target); err != nil {
			return err
		}
	}

	return nil
}

// updateEndpoint makes the records of a name and type match the targets.
// Records that already match are left alone, records with a stale value are
// rewritten, and extra or missing ones are deleted or added.
func (h *Handler) updateEndpoint(ep endpoint, existing recordIndex) error {
	domain, err := h.findDomain(ep.DNSName)
	if err != nil {
		return err
	}
	if len(ep.Targets) == 0 {
		return fmt.Errorf("no targets specified for update")
	}

	have := existing[recordKey(domain.Object, ep.DNSName, ep.RecordType)]
	if len(have) == 0 {
		h.Logger.Warn("No existing record for update, creating it", "dnsName", ep.DNSName, "recordType", ep.RecordType)
	}

	// An unset TTL means ExternalDNS has no wish about it, so kept records keep theirs.
	var stale []simply.Record
	for _, record := range have {
		if !hasTarget(ep.Targets, record) {
			stale = append(stale, record)
		} else if ep.RecordTTL != 0 && record.TTL != ep.RecordTTL {
			record.TTL = ep.RecordTTL
			if err := h.rewriteRecord(domain, ep, record, recordTarget(record)); err != nil {
				return err
			}
		}
	}

	var added []string
	for _, target := range ep.Targets {
		if hasData(have, target) || slices.Contains(added, target) {
			continue
		}
		added = append(added, target)

		if len(stale) == 0 {
			if err := h.addRecord(domain, ep, target); err != nil {
				return err
			}
			continue
		}
		record := stale[0]
		stale = stale[1:]
		if ep.RecordTTL != 0 {
			record.TTL = ep.RecordTTL
		}
		if err := h.rewriteRecord(domain, ep, record, target); err != nil {
			return err
		}
	}

	for _, record := range stale {
		if err := h.removeRecord(domain, record); err != nil {
			return err
		}
	}

	return nil
}

// deleteEndpoint deletes the records that hold the given targets, or every
// record of the name and type when no targets are given
func (h *Handler) deleteEndpoint(ep endpoint, existing recordIndex) error {
	domain, err := h.findDomain(ep.DNSName)
	if err != nil {
		h.Logger.Warn("No domain found for deletion, skipping", "dnsName", ep.DNSName, "error", err)
		return nil
	}

	deleted := 0
	for _, record := range existing[recordKey(domain.Object, ep.DNSName, ep.RecordType)] {
		if len(ep.Targets) > 0 && !hasTarget(ep.Targets, record) {
			continue
		}
		if err := h.removeRecord(domain, record); err != nil {
			return err
		}
		deleted++
	}
	if deleted == 0 {
		h.Logger.Warn("No matching record found for deletion, skipping", "dnsName", ep.DNSName, "recordType", ep.RecordType)
	}

	return nil
}

// newRecord builds the Simply record that holds target for ep. A zero id means a new record.
func newRecord(ep endpoint, target string, id, ttl int) (simply.Record, error) {
	priority, data, err := splitTarget(ep.RecordType, target)
	if err != nil {
		return simply.Record{}, err
	}
	return simply.Record{
		ID:       id,
		Type:     ep.RecordType,
		Name:     ep.DNSName,
		Data:     data,
		TTL:      ttl,
		Priority: priority,
		Comment:  DefaultComment,
	}, nil
}

func (h *Handler) addRecord(domain simply.Domain, ep endpoint, target string) error {
	record, err := newRecord(ep, target, 0, ttlOrDefault(ep.RecordTTL))
	if err != nil {
		return err
	}
	h.Logger.Info("Creating Simply.com record", "domain", domain.Object, "name", record.Name, "type", record.Type, "data", record.Data, "ttl", record.TTL)

	if err := h.Client.AddRecord(domain.Object, record); err != nil {
		return fmt.Errorf("failed to add record: %w", err)
	}
	return nil
}

// rewriteRecord updates an existing record to hold target, keeping record.TTL
func (h *Handler) rewriteRecord(domain simply.Domain, ep endpoint, record simply.Record, target string) error {
	updated, err := newRecord(ep, target, record.ID, record.TTL)
	if err != nil {
		return err
	}
	h.Logger.Info("Updating Simply.com record", "id", updated.ID, "domain", domain.Object, "name", updated.Name, "type", updated.Type, "data", updated.Data, "ttl", updated.TTL)

	if err := h.Client.UpdateRecord(domain.Object, updated); err != nil {
		return fmt.Errorf("failed to update record: %w", err)
	}
	return nil
}

func (h *Handler) removeRecord(domain simply.Domain, record simply.Record) error {
	h.Logger.Info("Deleting Simply.com record", "id", record.ID, "domain", domain.Object, "name", record.Name, "type", record.Type)

	if err := h.Client.DeleteRecord(domain.Object, record); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}
	return nil
}

// findDomain returns the configured domain that holds dnsName. The longest
// match wins, so a subdomain product such as www.example.com is preferred
// over example.com.
func (h *Handler) findDomain(dnsName string) (simply.Domain, error) {
	index := h.domains.Load().index
	name := strings.ToLower(strings.TrimSuffix(dnsName, "."))
	for candidate := name; candidate != ""; {
		if domain, ok := index[candidate]; ok {
			return domain, nil
		}
		dot := strings.IndexByte(candidate, '.')
		if dot < 0 {
			break
		}
		candidate = candidate[dot+1:]
	}
	return simply.Domain{}, fmt.Errorf("no managed Simply.com domain found for %s", dnsName)
}

// groupRecords turns the records of a domain into endpoints, one per name and
// type, with every record value as a target
func groupRecords(records []simply.Record, domain simply.Domain) []endpoint {
	var endpoints []endpoint
	position := make(map[string]int)
	for _, record := range records {
		name := recordDNSName(record, domain)
		key := recordKey(domain.Object, name, record.Type)
		if i, ok := position[key]; ok {
			endpoints[i].Targets = append(endpoints[i].Targets, recordTarget(record))
			continue
		}
		position[key] = len(endpoints)
		endpoints = append(endpoints, endpoint{
			DNSName:    name,
			RecordType: record.Type,
			Targets:    []string{recordTarget(record)},
			RecordTTL:  record.TTL,
		})
	}
	return endpoints
}

func ttlOrDefault(ttl int) int {
	if ttl == 0 {
		return DefaultTTL
	}
	return ttl
}

// recordTarget is a record value in ExternalDNS form: MX and SRV values start
// with the priority, which Simply.com keeps in a separate field.
func recordTarget(r simply.Record) string {
	if r.Priority == nil {
		return r.Data
	}
	return fmt.Sprintf("%d %s", *r.Priority, r.Data)
}

// splitTarget is the reverse of recordTarget
func splitTarget(recordType, target string) (priority *int, data string, err error) {
	if recordType != "MX" && recordType != "SRV" {
		return nil, target, nil
	}
	first, rest, found := strings.Cut(strings.TrimSpace(target), " ")
	n, convErr := strconv.Atoi(first)
	if !found || convErr != nil {
		return nil, "", fmt.Errorf("%s target %q must start with a priority", recordType, target)
	}
	return &n, strings.TrimSpace(rest), nil
}

// sameData compares record values, ignoring the quotes ExternalDNS puts around TXT values
func sameData(a, b string) bool {
	return strings.Trim(a, `"`) == strings.Trim(b, `"`)
}

func hasData(records []simply.Record, target string) bool {
	return slices.ContainsFunc(records, func(r simply.Record) bool { return sameData(recordTarget(r), target) })
}

func hasTarget(targets []string, record simply.Record) bool {
	return slices.ContainsFunc(targets, func(t string) bool { return sameData(t, recordTarget(record)) })
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
