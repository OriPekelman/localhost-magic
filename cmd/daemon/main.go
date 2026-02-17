package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"localhost-magic/internal/naming"
	"localhost-magic/internal/portscan"
	"localhost-magic/internal/probe"
	"localhost-magic/internal/storage"
)

// Service represents a discovered HTTP service
type Service struct {
	ID         string
	Name       string
	Port       int
	TargetHost string // Target IP/host (default: 127.0.0.1)
	PID        int
	ExePath    string
	Cwd        string
	Args       []string
	Proxy      *httputil.ReverseProxy
}

// Server manages the discovery and proxying of local services
type Server struct {
	store        *storage.Store
	generator    *naming.Generator
	services     map[string]*Service // key = name
	mu           sync.RWMutex
	pollInterval time.Duration
}

func main() {
	// Get storage path
	storePath := storage.DefaultStorePath()
	if len(os.Args) > 1 {
		storePath = os.Args[1]
	}

	// Initialize store
	store, err := storage.NewStore(storePath)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}

	// Create server
	srv := &Server{
		store:        store,
		generator:    naming.NewGenerator(),
		services:     make(map[string]*Service),
		pollInterval: 2 * time.Second,
	}

	// Load existing services into generator to avoid name collisions
	for _, record := range store.List() {
		srv.generator.GenerateName(record.ExePath, "", record.Args) // Mark name as used
		srv.services[record.Name] = &Service{
			ID:         record.ID,
			Name:       record.Name,
			Port:       record.Port,
			TargetHost: record.EffectiveTargetHost(),
			PID:        record.PID,
			ExePath:    record.ExePath,
			Cwd:        "",
			Args:       record.Args,
			Proxy:      nil, // Will be created on first use
		}
	}

	// Start discovery loop
	go srv.discoveryLoop()

	// Setup HTTP handler
	http.HandleFunc("/", srv.handleRequest)
	http.HandleFunc("/api/services", srv.handleAPIServices)
	http.HandleFunc("/api/rename", srv.handleAPIRename)
	http.HandleFunc("/api/blacklist", srv.handleAPIBlacklist)
	http.HandleFunc("/api/keep", srv.handleAPIKeep)

	log.Println("localhost-magic daemon starting...")
	log.Printf("Storage: %s", storePath)
	log.Println("Listening on :80 (requires root)")
	log.Println("Dashboard: http://localhost/ (when no hostname matches)")
	log.Fatal(http.ListenAndServe(":80", nil))
}

// discoveryLoop continuously scans for new services
func (s *Server) discoveryLoop() {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	// Run immediately on start
	s.discover()

	for range ticker.C {
		s.discover()
	}
}

// discover scans for listening ports and updates services
func (s *Server) discover() {
	listeners, err := portscan.Scan()
	if err != nil {
		log.Printf("Port scan failed: %v", err)
		return
	}

	now := time.Now()

	// Track which services we've seen this scan
	seenIDs := make(map[string]bool)
	seenNames := make(map[string]bool)

	for _, listener := range listeners {
		// Skip ourselves (port 80)
		if listener.Port == 80 {
			continue
		}

		// Skip blacklisted services
		if naming.IsBlacklisted(listener.ExePath, listener.Args) {
			continue
		}

		// Skip non-HTTP services (check if actually HTTP)
		if !probe.IsHTTP("127.0.0.1", listener.Port) {
			continue
		}

		// Compute identity hash
		id := naming.ComputeIdentityHash(listener.ExePath, listener.Args)
		seenIDs[id] = true

		// Check if we already know this service
		if existing, ok := s.store.Get(id); ok {
			seenNames[existing.Name] = true

			// Update if port, PID, or active status changed
			needsSave := false
			if existing.Port != listener.Port {
				existing.Port = listener.Port
				needsSave = true
			}
			if existing.PID != listener.PID {
				existing.PID = listener.PID
				needsSave = true
			}
			if !existing.IsActive {
				existing.IsActive = true
				needsSave = true
				log.Printf("Service reactivated: %s", existing.Name)
			}

			existing.LastSeen = now

			if needsSave {
				if err := s.store.Save(existing); err != nil {
					log.Printf("Failed to update service %s: %v", existing.Name, err)
				}
			}

			// Update runtime service
			s.mu.Lock()
			if svc, exists := s.services[existing.Name]; exists {
				svc.Port = listener.Port
				svc.PID = listener.PID
				svc.Cwd = listener.Cwd
			}
			s.mu.Unlock()
			continue
		}

		// Generate name for new service
		name := s.generator.GenerateName(listener.ExePath, listener.Cwd, listener.Args)

		// Create record
		record := &storage.ServiceRecord{
			ID:          id,
			Name:        name,
			Port:        listener.Port,
			PID:         listener.PID,
			ExePath:     listener.ExePath,
			Args:        listener.Args,
			UserDefined: false,
			IsActive:    true,
			LastSeen:    now,
			Keep:        false,
		}

		// Save to store
		if err := s.store.Save(record); err != nil {
			log.Printf("Failed to save service %s: %v", name, err)
			continue
		}

		// Add to runtime services
		s.mu.Lock()
		s.services[name] = &Service{
			ID:         id,
			Name:       name,
			Port:       listener.Port,
			TargetHost: "127.0.0.1",
			PID:        listener.PID,
			ExePath:    listener.ExePath,
			Cwd:        listener.Cwd,
			Args:       listener.Args,
		}
		s.mu.Unlock()

		seenNames[name] = true
		log.Printf("New service: %s -> 127.0.0.1:%d (%s)", name, listener.Port, listener.ExePath)
	}

	// Mark services as inactive if not seen
	s.mu.Lock()
	for name, svc := range s.services {
		if !seenNames[name] {
			if record, ok := s.store.Get(svc.ID); ok && record.IsActive {
				record.IsActive = false
				record.LastSeen = now
				s.store.Save(record)
				log.Printf("Service inactive: %s", name)
			}
		}
	}
	s.mu.Unlock()
}

// handleRequest routes HTTP requests to the appropriate service or dashboard
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Extract host without port
	host := r.Host
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}

	// If accessing by IP or localhost without specific subdomain, show dashboard
	if host == "localhost" || host == "127.0.0.1" || host == "" {
		s.serveDashboard(w, r)
		return
	}

	s.mu.RLock()
	service, ok := s.services[host]
	s.mu.RUnlock()

	if !ok {
		// No service found - show dashboard with message
		s.serveDashboardWithError(w, r, fmt.Sprintf("No service found for %s", host))
		return
	}

	// Create proxy on first use
	if service.Proxy == nil {
		targetURL := fmt.Sprintf("http://%s:%d", service.TargetHost, service.Port)
		target, err := url.Parse(targetURL)
		if err != nil {
			http.Error(w, "Invalid target URL", http.StatusInternalServerError)
			return
		}

		service.Proxy = httputil.NewSingleHostReverseProxy(target)
		// Custom error handler
		service.Proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error for %s: %v", host, err)
			http.Error(w, fmt.Sprintf("Service %s unavailable", host), http.StatusBadGateway)
		}
	}

	// Update Host header to match the backend
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Host = fmt.Sprintf("%s:%d", service.TargetHost, service.Port)

	service.Proxy.ServeHTTP(w, r)
}

// serveDashboard renders the admin dashboard HTML
func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	s.serveDashboardWithError(w, r, "")
}

// serveDashboardWithError renders the admin dashboard with an optional error message
func (s *Server) serveDashboardWithError(w http.ResponseWriter, r *http.Request, errorMsg string) {
	s.mu.RLock()
	services := make([]*Service, 0, len(s.services))
	for _, svc := range s.services {
		services = append(services, svc)
	}
	s.mu.RUnlock()

	data := struct {
		Services []*Service
		ErrorMsg string
		Hostname string
	}{
		Services: services,
		ErrorMsg: errorMsg,
		Hostname: r.Host,
	}

	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleAPIServices returns JSON list of services with health status
func (s *Server) handleAPIServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	services := make([]*Service, 0, len(s.services))
	for _, svc := range s.services {
		services = append(services, svc)
	}
	s.mu.RUnlock()

	// Check health of each service
	type ServiceWithHealth struct {
		*Service
		Healthy    bool   `json:"healthy"`
		StatusCode int    `json:"status_code"`
		StatusText string `json:"status_text"`
	}

	result := make([]ServiceWithHealth, 0, len(services))
	for _, svc := range services {
		swh := ServiceWithHealth{
			Service:    svc,
			Healthy:    false,
			StatusCode: 0,
			StatusText: "unknown",
		}

		// Quick health check
		client := &http.Client{Timeout: 2 * time.Second}
		targetHost := svc.TargetHost
		if targetHost == "" {
			targetHost = "127.0.0.1"
		}
		resp, err := client.Get(fmt.Sprintf("http://%s:%d", targetHost, svc.Port))
		if err != nil {
			swh.StatusText = "offline"
		} else {
			resp.Body.Close()
			swh.StatusCode = resp.StatusCode
			swh.StatusText = resp.Status
			// Consider healthy if status is 2xx or 3xx
			swh.Healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
		}

		result = append(result, swh)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleAPIRename handles rename requests
func (s *Server) handleAPIRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		OldName string `json:"oldName"`
		NewName string `json:"newName"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Ensure .localhost suffix
	if !strings.HasSuffix(req.NewName, ".localhost") {
		req.NewName = req.NewName + ".localhost"
	}

	// Find service by old name
	s.mu.Lock()
	defer s.mu.Unlock()

	var service *Service
	for _, svc := range s.services {
		if svc.Name == req.OldName {
			service = svc
			break
		}
	}

	if service == nil {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	// Update in store
	if err := s.store.UpdateName(service.ID, req.NewName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Update in memory
	delete(s.services, service.Name)
	service.Name = req.NewName
	s.services[service.Name] = service

	log.Printf("Renamed %s -> %s", req.OldName, req.NewName)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleAPIBlacklist handles blacklist requests
func (s *Server) handleAPIBlacklist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Type  string `json:"type"` // "pid", "path", "pattern"
		Value string `json:"value"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// TODO: Implement persistent blacklist storage
	// For now, just log and return success
	log.Printf("Blacklist request: %s = %s", req.Type, req.Value)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": fmt.Sprintf("Blacklisted %s: %s", req.Type, req.Value),
	})
}

// handleAPIKeep handles keep status updates
func (s *Server) handleAPIKeep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		Keep bool   `json:"keep"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Find the service
	s.mu.Lock()
	var service *Service
	for _, svc := range s.services {
		if svc.Name == req.Name {
			service = svc
			break
		}
	}
	s.mu.Unlock()

	if service == nil {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	// Update in store
	if record, ok := s.store.Get(service.ID); ok {
		record.Keep = req.Keep
		if err := s.store.Save(record); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	log.Printf("Updated keep status for %s: %v", req.Name, req.Keep)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// dashboardHTML is the admin dashboard template
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>localhost-magic</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
            background: #fff;
            color: #333;
            line-height: 1.5;
            padding: 40px 20px;
        }
        .container {
            max-width: 1200px;
            margin: 0 auto;
        }
        header {
            text-align: center;
            margin-bottom: 40px;
        }
        header h1 {
            font-size: 2em;
            font-weight: 600;
            color: #1a1a1a;
            margin-bottom: 8px;
        }
        header p {
            color: #666;
            font-size: 0.95em;
        }
        .card {
            background: #fff;
            border: 1px solid #e0e0e0;
            box-shadow: 0 1px 3px rgba(0,0,0,0.05);
        }
        .card-header {
            padding: 20px 24px;
            border-bottom: 1px solid #e0e0e0;
            background: #fafafa;
        }
        .card-header h2 {
            font-size: 1.1em;
            font-weight: 600;
            color: #1a1a1a;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            font-size: 0.9em;
        }
        th {
            text-align: left;
            padding: 12px 24px;
            font-weight: 600;
            color: #555;
            font-size: 0.75em;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            border-bottom: 1px solid #e0e0e0;
            background: #fafafa;
        }
        td {
            padding: 14px 24px;
            border-bottom: 1px solid #f0f0f0;
            vertical-align: middle;
        }
        tr:hover {
            background: #fafafa;
        }
        tr.inactive {
            opacity: 0.5;
        }
        .name-cell {
            display: flex;
            align-items: center;
            gap: 10px;
        }
        .status-dot {
            width: 10px;
            height: 10px;
            border-radius: 50%;
            flex-shrink: 0;
        }
        .status-dot.ok { background: #4caf50; }
        .status-dot.warning { background: #ff9800; }
        .status-dot.error { background: #f44336; }
        .status-dot.offline { background: #9e9e9e; }
        .service-link {
            color: #2196f3;
            text-decoration: none;
            font-weight: 500;
        }
        .service-link:hover {
            text-decoration: underline;
        }
        .service-link.inactive {
            color: #999;
        }
        .btn-icon {
            background: none;
            border: none;
            cursor: pointer;
            padding: 2px 4px;
            font-size: 0.85em;
            opacity: 0.5;
            transition: opacity 0.2s;
        }
        .btn-icon:hover {
            opacity: 1;
        }
        .status-badge {
            display: inline-block;
            padding: 4px 10px;
            font-size: 0.8em;
            font-weight: 500;
            border-radius: 3px;
        }
        .status-badge.ok {
            background: #e8f5e9;
            color: #2e7d32;
        }
        .status-badge.warning {
            background: #fff3e0;
            color: #ef6c00;
        }
        .status-badge.error {
            background: #ffebee;
            color: #c62828;
        }
        .status-badge.offline {
            background: #f5f5f5;
            color: #616161;
        }
        .command {
            font-family: 'Monaco', 'Menlo', 'Courier New', monospace;
            font-size: 0.8em;
            color: #555;
            background: #f5f5f5;
            padding: 4px 8px;
            border-radius: 3px;
            max-width: 400px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .keep-checkbox {
            display: flex;
            align-items: center;
            gap: 6px;
            cursor: pointer;
            font-size: 0.85em;
            color: #666;
        }
        .keep-checkbox input {
            cursor: pointer;
        }
        .btn {
            padding: 6px 14px;
            border: 1px solid #ddd;
            background: #fff;
            cursor: pointer;
            font-size: 0.8em;
            font-weight: 500;
            color: #555;
            transition: all 0.2s;
        }
        .btn:hover {
            background: #f5f5f5;
            border-color: #ccc;
        }
        .btn-danger {
            background: #f44336;
            color: #fff;
            border-color: #f44336;
        }
        .btn-danger:hover {
            background: #d32f2f;
            border-color: #d32f2f;
        }
        .empty-state {
            text-align: center;
            padding: 60px 20px;
            color: #999;
        }
        .modal {
            display: none;
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: rgba(0,0,0,0.5);
            z-index: 1000;
            justify-content: center;
            align-items: center;
        }
        .modal.active {
            display: flex;
        }
        .modal-content {
            background: white;
            padding: 24px;
            width: 90%;
            max-width: 400px;
            border: 1px solid #e0e0e0;
            box-shadow: 0 4px 20px rgba(0,0,0,0.15);
        }
        .modal-content h3 {
            margin-bottom: 20px;
            font-size: 1.1em;
        }
        .form-group {
            margin-bottom: 16px;
        }
        .form-group label {
            display: block;
            margin-bottom: 6px;
            font-size: 0.85em;
            font-weight: 500;
            color: #555;
        }
        .form-group input, .form-group select {
            width: 100%;
            padding: 8px 12px;
            border: 1px solid #ddd;
            font-size: 0.9em;
        }
        .form-group input:focus, .form-group select:focus {
            outline: none;
            border-color: #2196f3;
        }
        .modal-actions {
            display: flex;
            gap: 10px;
            justify-content: flex-end;
            margin-top: 20px;
        }
    </style>
</head>
<body>
    <div class="container">


        <div class="card">
            <div class="card-header">
                <h2>Discovered HTTP Servers</h2>
            </div>
            {{if .Services}}
            <table>
                <thead>
                    <tr>
                        <th>Name</th>
                        <th>Status</th>
                        <th>Port</th>
                        <th>PID</th>
                        <th>Command</th>
                        <th>Keep</th>
                        <th>Actions</th>
                    </tr>
                </thead>
                <tbody>
                    {{range .Services}}
                    <tr data-name="{{.Name}}" id="row-{{.Name}}">
                        <td>
                            <div class="name-cell">
                                <span class="status-dot ok" title="Checking..."></span>
                                http://<a href="http://{{.Name}}" class="service-link" target="_blank" id="link-{{.Name}}">{{.Name}}</a>
                                <button class="btn-icon" onclick="openRenameModal('{{.Name}}')" title="Rename">Edit</button>
                            </div>
                        </td>
                        <td>
                            <span class="status-badge ok" data-name="{{.Name}}">HTTP</span>
                        </td>
                        <td>{{.Port}}</td>
                        <td>{{.PID}}</td>
                        <td><pre class="command">{{.ExePath}}</pre></td>
                        <td>
                            <label class="keep-checkbox">
                                <input type="checkbox" id="keep-{{.Name}}" onchange="toggleKeep('{{.Name}}')">
                                <span>Keep</span>
                            </label>
                        </td>
                        <td>
                            <button class="btn btn-danger" onclick="openBlacklistModal('{{.Name}}', {{.PID}}, '{{.ExePath}}')">Blacklist</button>
                        </td>
                    </tr>
                    {{end}}
                </tbody>
            </table>
            {{else}}
            <div class="empty-state">
                <p>No services found. Start a local HTTP server to see it here.</p>
            </div>
            {{end}}
        </div>
    </div>

    <!-- Rename Modal -->
    <div id="renameModal" class="modal">
        <div class="modal-content">
            <h3>Rename Service</h3>
            <div class="form-group">
                <label>Current Name</label>
                <input type="text" id="currentName" readonly>
            </div>
            <div class="form-group">
                <label>New Name</label>
                <input type="text" id="newName" placeholder="myapp.localhost">
            </div>
            <div class="modal-actions">
                <button class="btn" onclick="closeModal('renameModal')">Cancel</button>
                <button class="btn" onclick="confirmRename()" style="background:#2196f3;color:#fff;border-color:#2196f3;">Rename</button>
            </div>
        </div>
    </div>

    <!-- Blacklist Modal -->
    <div id="blacklistModal" class="modal">
        <div class="modal-content">
            <h3>Blacklist Service</h3>
            <div class="form-group">
                <label>Blacklist Type</label>
                <select id="blacklistType"></select>
            </div>
            <div class="form-group">
                <label>Value</label>
                <input type="text" id="blacklistValue" readonly>
            </div>
            <div class="modal-actions">
                <button class="btn" onclick="closeModal('blacklistModal')">Cancel</button>
                <button class="btn btn-danger" onclick="confirmBlacklist()">Blacklist</button>
            </div>
        </div>
    </div>

    <script>
        let currentService = {};
        const keptServices = JSON.parse(localStorage.getItem('keptServices') || '[]');

        document.addEventListener('DOMContentLoaded', () => {
            keptServices.forEach(name => {
                const checkbox = document.getElementById('keep-' + name);
                if (checkbox) checkbox.checked = true;
            });
            fetchStatus();
        });

        function openRenameModal(name) {
            currentService.oldName = name;
            document.getElementById('currentName').value = name;
            document.getElementById('newName').value = '';
            document.getElementById('renameModal').classList.add('active');
        }

        function openBlacklistModal(name, pid, exePath) {
            currentService = { name, pid, exePath };
            document.getElementById('blacklistValue').value = pid;
            
            const typeSelect = document.getElementById('blacklistType');
            typeSelect.innerHTML = '';
            
            const options = [
                { value: 'pid', text: 'By PID (' + pid + ')' },
                { value: 'path', text: 'By Path (' + exePath.substring(0, 50) + '...)' },
                { value: 'pattern', text: 'By Pattern (regex)' }
            ];
            
            options.forEach(opt => {
                const option = document.createElement('option');
                option.value = opt.value;
                option.textContent = opt.text;
                typeSelect.appendChild(option);
            });
            
            typeSelect.onchange = function() {
                const val = typeSelect.value;
                if (val === 'pid') document.getElementById('blacklistValue').value = pid;
                if (val === 'path') document.getElementById('blacklistValue').value = exePath;
                if (val === 'pattern') document.getElementById('blacklistValue').value = '';
                document.getElementById('blacklistValue').readOnly = (val !== 'pattern');
            };
            
            document.getElementById('blacklistModal').classList.add('active');
        }

        function closeModal(modalId) {
            document.getElementById(modalId).classList.remove('active');
        }

        function toggleKeep(name) {
            const checkbox = document.getElementById('keep-' + name);
            const index = keptServices.indexOf(name);
            
            if (checkbox.checked) {
                if (index === -1) keptServices.push(name);
            } else {
                if (index > -1) keptServices.splice(index, 1);
            }
            
            localStorage.setItem('keptServices', JSON.stringify(keptServices));
        }

        async function confirmRename() {
            const newName = document.getElementById('newName').value;
            if (!newName) return;

            try {
                const response = await fetch('/api/rename', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        oldName: currentService.oldName,
                        newName: newName
                    })
                });

                if (response.ok) {
                    location.reload();
                } else {
                    alert('Failed to rename: ' + await response.text());
                }
            } catch (err) {
                alert('Error: ' + err.message);
            }
        }

        async function confirmBlacklist() {
            const type = document.getElementById('blacklistType').value;
            const value = document.getElementById('blacklistValue').value;

            try {
                const response = await fetch('/api/blacklist', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ type, value })
                });

                if (response.ok) {
                    location.reload();
                } else {
                    alert('Failed to blacklist: ' + await response.text());
                }
            } catch (err) {
                alert('Error: ' + err.message);
            }
        }

        document.querySelectorAll('.modal').forEach(modal => {
            modal.addEventListener('click', (e) => {
                if (e.target === modal) closeModal(modal.id);
            });
        });

        async function fetchStatus() {
            try {
                const response = await fetch('/api/services');
                const services = await response.json();
                updateServiceStatuses(services);
            } catch (err) {
                console.error('Failed to fetch service status:', err);
            }
        }

        setInterval(fetchStatus, 3000);

        function updateServiceStatuses(services) {
            const activeServices = new Map(services.map(s => [s.Name, s]));
            
            document.querySelectorAll('tr[data-name]').forEach(row => {
                const name = row.getAttribute('data-name');
                const service = activeServices.get(name);
                const isKept = keptServices.includes(name);
                
                if (!service) {
                    if (isKept) {
                        row.classList.add('inactive');
                        const link = document.getElementById('link-' + name);
                        if (link) link.classList.add('inactive');
                        updateStatus(row, 'offline', 'INACTIVE');
                    } else {
                        row.style.display = 'none';
                    }
                    return;
                }
                
                const code = service.status_code || 0;
                
                if (code >= 200 && code < 400) {
                    updateStatus(row, 'ok', code);
                } else if (code >= 400 && code < 500) {
                    updateStatus(row, 'warning', code);
                } else if (code >= 500) {
                    updateStatus(row, 'error', code);
                } else {
                    updateStatus(row, 'offline', 'OFFLINE');
                }
            });
        }
        
        function updateStatus(row, statusClass, text) {
            const dot = row.querySelector('.status-dot');
            const badge = row.querySelector('.status-badge');
            
            if (dot) {
                dot.className = 'status-dot ' + statusClass;
            }
            if (badge) {
                badge.className = 'status-badge ' + statusClass;
                badge.textContent = text;
            }
        }
    </script>
</body>
</html>
`
