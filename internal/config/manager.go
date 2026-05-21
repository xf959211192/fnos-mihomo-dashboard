package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Manager safely reads/writes mihomo's config.yaml, treating proxy-providers as the
// "subscription" surface that we mutate, while preserving all other user-managed fields.
type Manager struct {
	Path string
	mu   sync.Mutex
}

func New(path string) *Manager {
	return &Manager{Path: path}
}

// Read returns the raw config as a map (preserving order via yaml.Node would need more work,
// but for our limited mutation surface a map is fine).
func (m *Manager) Read() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readUnsafe()
}

func (m *Manager) readUnsafe() (map[string]any, error) {
	b, err := os.ReadFile(m.Path)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.Path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// SetSubscription replaces the `proxy-providers.fnos-subscription` entry with the given URL.
// All other proxy-providers (if any) are preserved. Also ensures the default proxy-group
// includes this provider via `use:`.
func (m *Manager) SetSubscription(url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.readUnsafe()
	if err != nil {
		return err
	}

	providers, _ := cfg["proxy-providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}

	dir := filepath.Dir(m.Path)
	providerPath := filepath.Join(dir, "providers", "fnos-subscription.yaml")

	providers["fnos-subscription"] = map[string]any{
		"type":     "http",
		"url":      url,
		"interval": 86400, // 24h
		"path":     providerPath,
		"health-check": map[string]any{
			"enable":   true,
			"url":      "http://www.gstatic.com/generate_204",
			"interval": 300,
		},
	}
	cfg["proxy-providers"] = providers

	// Ensure default PROXY group uses this provider
	groups, _ := cfg["proxy-groups"].([]any)
	hasFnosGroup := false
	for i, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := gm["name"].(string); name == "PROXY" {
			use, _ := gm["use"].([]any)
			has := false
			for _, u := range use {
				if s, _ := u.(string); s == "fnos-subscription" {
					has = true
					break
				}
			}
			if !has {
				use = append(use, "fnos-subscription")
				gm["use"] = use
			}
			groups[i] = gm
			hasFnosGroup = true
			break
		}
	}
	if !hasFnosGroup {
		groups = append([]any{map[string]any{
			"name":    "PROXY",
			"type":    "select",
			"use":     []any{"fnos-subscription"},
			"proxies": []any{"DIRECT"},
		}}, groups...)
	}
	cfg["proxy-groups"] = groups

	// Ensure at least one rule exists (MATCH,PROXY)
	if rules, _ := cfg["rules"].([]any); len(rules) == 0 {
		cfg["rules"] = []any{"MATCH,PROXY"}
	}

	applyFnOSOverrides(cfg)

	return m.writeUnsafe(cfg)
}

// subURLPath returns the sidecar file that stores the current subscription URL.
// We keep it separate from config.yaml because the proxy-provider now uses
// type: file (mihomo doesn't need to know the original remote URL).
func (m *Manager) subURLPath() string {
	return filepath.Join(filepath.Dir(m.Path), ".fnos-subscription.url")
}

// GetSubscription returns the current fnos-subscription URL (or empty string).
func (m *Manager) GetSubscription() (string, error) {
	b, err := os.ReadFile(m.subURLPath())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SetSubscriptionFromURL persists the subscription URL, writes the supplied
// proxies list to providers/fnos-subscription.yaml (the file mihomo will
// actually read), and updates config.yaml so the PROXY group references it
// via a `type: file` proxy-provider. Applies fnOS overrides at the end.
func (m *Manager) SetSubscriptionFromURL(url string, proxies []any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(proxies) == 0 {
		return fmt.Errorf("refusing to save an empty proxies list")
	}

	dir := filepath.Dir(m.Path)
	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		return err
	}
	providerFile := filepath.Join(providersDir, "fnos-subscription.yaml")
	providerYaml, err := yaml.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		return err
	}
	if err := os.WriteFile(providerFile, providerYaml, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(m.subURLPath(), []byte(url), 0o644); err != nil {
		return err
	}

	cfg, err := m.readUnsafe()
	if err != nil {
		return err
	}

	providers, _ := cfg["proxy-providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	providers["fnos-subscription"] = map[string]any{
		"type": "file",
		"path": providerFile,
		"health-check": map[string]any{
			"enable":   true,
			"url":      "http://www.gstatic.com/generate_204",
			"interval": 300,
		},
	}
	cfg["proxy-providers"] = providers

	groups, _ := cfg["proxy-groups"].([]any)
	hasFnosGroup := false
	for i, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := gm["name"].(string); name == "PROXY" {
			use, _ := gm["use"].([]any)
			has := false
			for _, u := range use {
				if s, _ := u.(string); s == "fnos-subscription" {
					has = true
					break
				}
			}
			if !has {
				use = append(use, "fnos-subscription")
				gm["use"] = use
			}
			groups[i] = gm
			hasFnosGroup = true
			break
		}
	}
	if !hasFnosGroup {
		groups = append([]any{map[string]any{
			"name":    "PROXY",
			"type":    "select",
			"use":     []any{"fnos-subscription"},
			"proxies": []any{"DIRECT"},
		}}, groups...)
	}
	cfg["proxy-groups"] = groups

	if rules, _ := cfg["rules"].([]any); len(rules) == 0 {
		cfg["rules"] = []any{"MATCH,PROXY"}
	}

	applyFnOSOverrides(cfg)

	return m.writeUnsafe(cfg)
}

func (m *Manager) writeUnsafe(cfg map[string]any) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// Atomic write
	tmp := m.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.Path)
}


// applyFnOSOverrides ensures fnOS 旁路由 / transparent-proxy safety:
//   1. profile.store-selected + store-fake-ip = true (persist UI selection)
//   2. sniffer enabled (TLS/HTTP/QUIC) — without it, transparent-proxy rules
//      fall through to MATCH/Final (see ../docs/mihomo.md §2.4)
//   3. tun.inet4-route-exclude-address strips 198.18.0.0/16 (fake-ip must
//      be handled by TUN, see ../docs/mihomo.md §2.1)
func applyFnOSOverrides(cfg map[string]any) {
	// 1. profile (always force)
	profile, _ := cfg["profile"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
	}
	profile["store-selected"] = true
	profile["store-fake-ip"] = true
	cfg["profile"] = profile

	// 2. sniffer (only set if user didn't configure)
	if _, ok := cfg["sniffer"]; !ok {
		cfg["sniffer"] = map[string]any{
			"enable": true,
			"sniff": map[string]any{
				"TLS":  map[string]any{"ports": []any{443, 8443}},
				"HTTP": map[string]any{"ports": []any{80, 8080, 8880}, "override-destination": true},
				"QUIC": map[string]any{"ports": []any{443, 8443}},
			},
			"parse-pure-ip":        true,
			"override-destination": true,
			"skip-domain":          []any{"+.apple.com", "+.icloud.com"},
		}
	}

	// 3. tun: strip 198.18.x from inet4-route-exclude-address
	if tun, ok := cfg["tun"].(map[string]any); ok {
		if excludes, ok := tun["inet4-route-exclude-address"].([]any); ok {
			filtered := make([]any, 0, len(excludes))
			for _, e := range excludes {
				if s, ok := e.(string); ok && strings.HasPrefix(s, "198.18.") {
					continue
				}
				filtered = append(filtered, e)
			}
			tun["inet4-route-exclude-address"] = filtered
			cfg["tun"] = tun
		}
	}
}

// AppliedOverrides returns a human-readable list of overrides applied to the config.
func (m *Manager) AppliedOverrides() []map[string]any {
	return []map[string]any{
		{"key": "profile.store-selected", "desc": "持久化策略组选择 (重启保留)", "value": true},
		{"key": "profile.store-fake-ip", "desc": "持久化 fake-ip 池", "value": true},
		{"key": "sniffer", "desc": "TLS/HTTP/QUIC Sniffer (透明代理规则匹配)", "value": "enabled"},
		{"key": "tun.inet4-route-exclude-address", "desc": "剔除 198.18.0.0/16 (fake-ip 段必须由 TUN 接管)", "value": "sanitized"},
	}
}

// Backup copies config.yaml to config.yaml.bak (atomic). Returns path of backup.
func (m *Manager) Backup() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.Path)
	if err != nil {
		return "", err
	}
	bak := m.Path + ".bak"
	tmp := bak + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, bak); err != nil {
		return "", err
	}
	return bak, nil
}

// RestoreFromBackup overwrites config.yaml with config.yaml.bak.
func (m *Manager) RestoreFromBackup() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bak := m.Path + ".bak"
	data, err := os.ReadFile(bak)
	if err != nil {
		return err
	}
	return os.WriteFile(m.Path, data, 0o644)
}

// HasBackup reports whether a previous backup file exists.
func (m *Manager) HasBackup() bool {
	_, err := os.Stat(m.Path + ".bak")
	return err == nil
}

