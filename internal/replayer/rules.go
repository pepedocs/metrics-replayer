package replayer

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

func (r *Replayer) handleRules(w http.ResponseWriter, req *http.Request) {
	name := nameFromPath("/rules/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case http.MethodPost:
		r.handleRulesCreate(w, req, name)
	case http.MethodDelete:
		r.handleRulesDelete(w, req, name)
	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (r *Replayer) handleRulesCreate(w http.ResponseWriter, req *http.Request, name string) {
	if r.rulesDir == "" || r.prometheusURL == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if r.stripFor {
		body = stripForFromRules(body)
	}

	// ?backfill=<duration>[&step=<duration>] also evaluates the rules over
	// history. Parse it before touching any files so bad input changes nothing.
	var backfill, step time.Duration
	if v := req.URL.Query().Get("backfill"); v != "" {
		d, err := model.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, "invalid backfill duration", http.StatusBadRequest)
			return
		}
		backfill = time.Duration(d)
		step = 30 * time.Second
		if v := req.URL.Query().Get("step"); v != "" {
			d, err := model.ParseDuration(v)
			if err != nil || d <= 0 {
				http.Error(w, "invalid step", http.StatusBadRequest)
				return
			}
			step = time.Duration(d)
		}
		if r.remoteWriteURL == "" {
			http.Error(w, "rule backfill requires remote write", http.StatusServiceUnavailable)
			return
		}
	}
	// Backfilled results end where live evaluation after the reload begins.
	loadedAt := time.Now()

	path := filepath.Join(r.rulesDir, name+".yaml")
	if err := os.WriteFile(path, body, 0644); err != nil {
		http.Error(w, "write rule file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.reloadPrometheus(); err != nil {
		os.Remove(path)
		http.Error(w, "prometheus reload failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("created rule %q (%d bytes)", name, len(body))

	if backfill > 0 {
		samples, err := r.backfillRules(body, loadedAt.Add(-step), backfill, step)
		if err != nil {
			http.Error(w, "rules loaded, but backfill failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("backfilled rule %q: %d samples over %s (step=%s)", name, samples, backfill, step)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "backfilled %d rule samples over %s\n", samples, backfill)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (r *Replayer) handleRulesDelete(w http.ResponseWriter, req *http.Request, name string) {
	if r.rulesDir == "" || r.prometheusURL == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	path := filepath.Join(r.rulesDir, name+".yaml")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove rule file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.reloadPrometheus(); err != nil {
		log.Printf("prometheus reload after delete failed: %v", err)
	}

	log.Printf("deleted rule %q", name)
	w.WriteHeader(http.StatusOK)
}

func (r *Replayer) handleRulesList(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if r.rulesDir == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	entries, err := os.ReadDir(r.rulesDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(names)
}

func stripForFromRules(data []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// not JSON, try YAML-style line removal
		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "for:") {
				continue
			}
			lines = append(lines, line)
		}
		return []byte(strings.Join(lines, "\n"))
	}
	// JSON path (unlikely but handle it)
	return data
}

func (r *Replayer) reloadPrometheus() error {
	resp, err := http.Post(r.prometheusURL+"/-/reload", "", nil)
	if err != nil {
		return fmt.Errorf("reload request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
