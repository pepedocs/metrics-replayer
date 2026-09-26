package replayer

import (
	"io"
	"log"
	"net/http"
)

func (r *Replayer) handleRegister(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := nameFromPath("/register/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if r.scrapesDir != "" && !validStreamName.MatchString(name) {
		http.Error(w, "invalid name: must match "+validStreamName.String(), http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	if _, exists := r.streams[name]; exists {
		r.mu.Unlock()
		http.Error(w, "already registered", http.StatusConflict)
		return
	}
	r.streams[name] = nil
	r.mu.Unlock()

	// The stream must exist before Prometheus starts scraping it, and the lock
	// must not be held while waiting for that scrape.
	if r.scrapesDir != "" {
		if err := r.addScrapeConfig(name); err != nil {
			r.mu.Lock()
			delete(r.streams, name)
			r.mu.Unlock()
			http.Error(w, "add scrape config: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	log.Printf("registered stream %q", name)
	w.WriteHeader(http.StatusCreated)
}

func (r *Replayer) handlePush(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := nameFromPath("/push/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.streams[name]; !exists {
		http.Error(w, "not registered", http.StatusNotFound)
		return
	}
	r.streams[name] = append(r.streams[name], string(body))
	log.Printf("pushed %d bytes to stream %q (queue depth: %d)", len(body), name, len(r.streams[name]))
	w.WriteHeader(http.StatusOK)
}

func (r *Replayer) handleMetrics(w http.ResponseWriter, req *http.Request) {
	name := nameFromPath("/metrics/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	queue, exists := r.streams[name]
	var data string
	if exists && len(queue) > 0 {
		data = queue[0]
		r.streams[name] = queue[1:]
	}
	r.mu.Unlock()

	if !exists {
		http.Error(w, "not registered", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(data))
}
