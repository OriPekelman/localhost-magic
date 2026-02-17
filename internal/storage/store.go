package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ServiceRecord represents a persisted service mapping
type ServiceRecord struct {
	ID          string    `json:"id"`                    // Hash of exe+args
	Name        string    `json:"name"`                  // Assigned DNS name
	Port        int       `json:"port"`                  // Current port
	TargetHost  string    `json:"target_host,omitempty"` // Target IP/host (default: 127.0.0.1)
	PID         int       `json:"pid"`                   // Current PID
	ExePath     string    `json:"exe_path"`              // Real path to executable
	Args        []string  `json:"args"`                  // Command line arguments
	UserDefined bool      `json:"user_defined"`          // Whether name was manually set
	IsActive    bool      `json:"is_active"`             // Whether service is currently running
	LastSeen    time.Time `json:"last_seen"`             // Last time service was detected
	Keep        bool      `json:"keep"`                  // Whether to keep even when inactive
}

// EffectiveTargetHost returns the target host, defaulting to 127.0.0.1
func (r *ServiceRecord) EffectiveTargetHost() string {
	if r.TargetHost == "" {
		return "127.0.0.1"
	}
	return r.TargetHost
}

// Store manages persistence of service name mappings
type Store struct {
	path    string
	records map[string]*ServiceRecord // key = ID
	names   map[string]string         // name -> ID mapping
}

// NewStore creates a new store with the given file path
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:    path,
		records: make(map[string]*ServiceRecord),
		names:   make(map[string]string),
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	// Load existing data
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load store: %w", err)
	}

	return s, nil
}

// Get returns a record by ID
func (s *Store) Get(id string) (*ServiceRecord, bool) {
	r, ok := s.records[id]
	return r, ok
}

// GetByName returns a record by its assigned name
func (s *Store) GetByName(name string) (*ServiceRecord, bool) {
	id, ok := s.names[name]
	if !ok {
		return nil, false
	}
	return s.Get(id)
}

// Save stores or updates a record
func (s *Store) Save(record *ServiceRecord) error {
	// Remove old name mapping if exists
	if old, ok := s.records[record.ID]; ok {
		delete(s.names, old.Name)
	}

	s.records[record.ID] = record
	s.names[record.Name] = record.ID

	return s.persist()
}

// UpdateName changes the name of a service
func (s *Store) UpdateName(id string, newName string) error {
	record, ok := s.records[id]
	if !ok {
		return fmt.Errorf("record not found: %s", id)
	}

	// Check if new name is already used by another service
	if existingID, exists := s.names[newName]; exists && existingID != id {
		return fmt.Errorf("name %s is already in use", newName)
	}

	// Remove old name mapping
	delete(s.names, record.Name)

	// Update record
	record.Name = newName
	record.UserDefined = true
	s.names[newName] = id

	return s.persist()
}

// List returns all records
func (s *Store) List() []*ServiceRecord {
	result := make([]*ServiceRecord, 0, len(s.records))
	for _, r := range s.records {
		result = append(result, r)
	}
	return result
}

// IsNameAvailable checks if a name is not in use
func (s *Store) IsNameAvailable(name string) bool {
	_, exists := s.names[name]
	return !exists
}

// UpdateKeep changes the keep status of a service
func (s *Store) UpdateKeep(id string, keep bool) error {
	record, ok := s.records[id]
	if !ok {
		return fmt.Errorf("record not found: %s", id)
	}

	record.Keep = keep
	return s.persist()
}

// AddManualService adds a service manually (for services not currently running)
func (s *Store) AddManualService(name string, port int, targetHost string) (*ServiceRecord, error) {
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}

	// Generate a unique ID for this manual entry
	id := fmt.Sprintf("manual-%s-%s-%d", name, targetHost, port)

	// Check if name is available
	if _, exists := s.names[name]; exists {
		return nil, fmt.Errorf("name %s is already in use", name)
	}

	record := &ServiceRecord{
		ID:          id,
		Name:        name,
		Port:        port,
		TargetHost:  targetHost,
		PID:         0,
		ExePath:     "manual",
		Args:        []string{},
		UserDefined: true,
		IsActive:    false,
		Keep:        true, // Manual entries are automatically kept
		LastSeen:    time.Now(),
	}

	if err := s.Save(record); err != nil {
		return nil, err
	}

	return record, nil
}

// load reads the store from disk
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}

	var records []*ServiceRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}

	for _, r := range records {
		s.records[r.ID] = r
		s.names[r.Name] = r.ID
	}

	return nil
}

// persist writes the store to disk
func (s *Store) persist() error {
	records := s.List()
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, data, 0644)
}

// DefaultStorePath returns the default storage path
func DefaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "localhost-magic", "services.json")
}
