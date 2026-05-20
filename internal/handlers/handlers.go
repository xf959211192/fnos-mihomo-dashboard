package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"

	"github.com/conversun/fnos-mihomo-dashboard/internal/config"
	"github.com/conversun/fnos-mihomo-dashboard/internal/mihomo"
	"github.com/conversun/fnos-mihomo-dashboard/internal/subscription"
)

type Handlers struct {
	cfg     *config.Manager
	logFile string
	mihomo  *mihomo.Client
	confPath string
	subInfo *subscription.Cache
}

func New(configFile, logFile string, mihomoAPI *url.URL) *Handlers {
	return &Handlers{
		cfg:      config.New(configFile),
		logFile:  logFile,
		mihomo:   mihomo.New(mihomoAPI),
		confPath: configFile,
		subInfo:  subscription.NewCache(),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// GET  /api/subscription  → {url: "..."}
// POST /api/subscription  body {url: "..."} → set + trigger mihomo reload
func (h *Handlers) Subscription(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		u, err := h.cfg.GetSubscription()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"url": u})

	case http.MethodPost:
		var body struct{ URL string `json:"url"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, err)
			return
		}
		if body.URL == "" {
			writeErr(w, 400, errEmptyURL)
			return
		}
		// Pre-flight: cheaply reject obviously bad URLs (HTML pages, 4xx/5xx)
		if err := subscription.Validate(body.URL); err != nil {
			writeErr(w, 400, err)
			return
		}
		// Health-checked write: backup current config first; if mihomo
		// reload fails after applying changes, automatically roll back.
		bakPath, _ := h.cfg.Backup()
		if err := h.cfg.SetSubscription(body.URL); err != nil {
			writeErr(w, 500, err)
			return
		}
		if err := h.mihomo.ReloadConfigPath(h.confPath); err != nil {
			// rollback
			rbErr := h.cfg.RestoreFromBackup()
			_ = h.mihomo.ReloadConfigPath(h.confPath)
			writeJSON(w, 502, map[string]any{
				"error":          "mihomo reload failed; config rolled back",
				"detail":         err.Error(),
				"rollback_error": rollbackErr(rbErr),
				"backup":         bakPath,
			})
			return
		}
		// Best-effort: fetch subscription-userinfo for the UI
		if info, ferr := subscription.Fetch(body.URL); ferr == nil {
			h.subInfo.Put(body.URL, info)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// GET /api/status → {version: ..., currentProxy: ...}
func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if v, err := h.mihomo.Version(); err == nil {
		out["version"] = v
	} else {
		out["version_error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

// GET /api/logs → last ~100 lines of mihomo.log
func (h *Handlers) Logs(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(h.logFile)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	// Return last 100 lines
	lines := splitLastN(b, 100)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(lines)
}

// POST /api/reload — force mihomo to reload config.yaml from disk
func (h *Handlers) Reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := h.mihomo.ReloadConfigPath(h.confPath); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// GET /api/subscription/info — returns cached subscription-userinfo (used / total / expire)
func (h *Handlers) SubscriptionInfo(w http.ResponseWriter, r *http.Request) {
	url, err := h.cfg.GetSubscription()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if url == "" {
		writeJSON(w, 200, map[string]any{"present": false})
		return
	}
	info := h.subInfo.Get(url)
	if info == nil {
		// fetch on-demand
		if fetched, ferr := subscription.Fetch(url); ferr == nil {
			h.subInfo.Put(url, fetched)
			info = fetched
		}
	}
	if info == nil {
		writeJSON(w, 200, map[string]any{"present": true, "info": nil, "url": url})
		return
	}
	writeJSON(w, 200, map[string]any{"present": true, "info": info})
}

// POST /api/subscription/refresh — ask mihomo to re-pull the proxy-provider URL right now
func (h *Handlers) SubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := h.mihomo.UpdateProvider("fnos-subscription"); err != nil {
		writeErr(w, 502, err)
		return
	}
	// refresh subscription-userinfo too
	if url, _ := h.cfg.GetSubscription(); url != "" {
		if info, ferr := subscription.Fetch(url); ferr == nil {
			h.subInfo.Put(url, info)
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// GET /api/config — return raw config.yaml content (post-override)
func (h *Handlers) Config(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(h.confPath)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// GET /api/overrides — list fnOS overrides applied to every saved config
func (h *Handlers) Overrides(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"overrides": h.cfg.AppliedOverrides(),
	})
}

// splitLastN returns the last n lines of b (joined with newline).
func splitLastN(b []byte, n int) []byte {
	count := 0
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			count++
			if count >= n {
				return b[i+1:]
			}
		}
	}
	return b
}

func rollbackErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type errStr string

func (e errStr) Error() string { return string(e) }

var errEmptyURL = errStr("subscription url cannot be empty")
