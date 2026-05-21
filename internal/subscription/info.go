package subscription

import (
	"gopkg.in/yaml.v3"
	"fmt"
	"io"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info captures the airline-style subscription metadata returned via the
// `subscription-userinfo` HTTP response header by Clash subscription endpoints.
//
//	subscription-userinfo: upload=44137197; download=24471430; total=10737418240; expire=1671235200
type Info struct {
	Upload    int64 `json:"upload"`
	Download  int64 `json:"download"`
	Total     int64 `json:"total"`
	Expire    int64 `json:"expire"`     // unix seconds; 0 if absent
	UpdatedAt int64 `json:"updated_at"` // unix seconds when this Info was fetched
	URL       string `json:"url"`
}

var ErrNoUserInfo = errors.New("subscription URL did not return subscription-userinfo header")

// Cache holds the most recent Info per URL.
type Cache struct {
	mu    sync.RWMutex
	items map[string]*Info
}

func NewCache() *Cache {
	return &Cache{items: map[string]*Info{}}
}

func (c *Cache) Get(url string) *Info {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.items[url]
}

func (c *Cache) Put(url string, info *Info) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[url] = info
}

// Fetch performs a GET with a Clash-style User-Agent (most airlines key on it)
// and parses the subscription-userinfo header without downloading the full body.
func Fetch(url string) (*Info, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash.meta")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	header := resp.Header.Get("subscription-userinfo")
	if header == "" {
		return nil, ErrNoUserInfo
	}
	info := &Info{UpdatedAt: time.Now().Unix(), URL: url}
	for _, kv := range strings.Split(header, ";") {
		kv = strings.TrimSpace(kv)
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		val, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(parts[0]) {
		case "upload":
			info.Upload = val
		case "download":
			info.Download = val
		case "total":
			info.Total = val
		case "expire":
			info.Expire = val
		}
	}
	return info, nil
}

// Validate quickly checks if a URL is reachable and returns something that
// at least *looks* like a Clash yaml subscription. Returns nil on success.
//
// We can't fully validate without consuming the entire body (which can be
// large) but we cheaply reject:
//   - non-2xx HTTP responses
//   - HTML pages (e.g. landing pages, 404 fallbacks like example.com/*)
//   - empty bodies
func Validate(url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "clash.meta")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errors.New("subscription URL returned HTTP " + strconv.Itoa(resp.StatusCode))
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		return errors.New("subscription URL returned an empty body")
	}
	head := strings.ToLower(strings.TrimSpace(string(buf[:n])))
	if strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html") {
		return errors.New("subscription URL returned an HTML page (not a Clash yaml). Verify the URL is correct")
	}
	if !strings.ContainsAny(head, ":-") {
		return errors.New("subscription URL response does not look like a yaml config")
	}
	return nil
}

// FetchProxies downloads the subscription URL, parses it as yaml, extracts
// the `proxies` list, and returns it along with any subscription-userinfo.
//
// This accepts both shapes a subscription endpoint may return:
//   - bare proxy-provider yaml: `proxies: [...]`
//   - full Clash config yaml:   `proxies: [...]; proxy-groups: [...]; rules: [...]; ...`
//
// We always extract just `proxies`, which is what mihomo's proxy-providers
// `type: file` expects. This is the key insight that lets users paste their
// airline's standard "Clash subscription URL" without manual editing.
func FetchProxies(rawURL string) (proxies []any, info *Info, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "clash.meta")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("subscription URL returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MB safety cap
	if err != nil {
		return nil, nil, err
	}
	var data map[string]any
	if err := yaml.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("parse subscription yaml: %w", err)
	}
	rawProxies, _ := data["proxies"].([]any)
	if len(rawProxies) == 0 {
		return nil, nil, errors.New("subscription yaml has no `proxies` field (empty / not a Clash subscription?)")
	}
	// subscription-userinfo header (best-effort)
	if header := resp.Header.Get("subscription-userinfo"); header != "" {
		info = &Info{UpdatedAt: time.Now().Unix(), URL: rawURL}
		for _, kv := range strings.Split(header, ";") {
			kv = strings.TrimSpace(kv)
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			val, perr := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if perr != nil {
				continue
			}
			switch strings.TrimSpace(parts[0]) {
			case "upload":
				info.Upload = val
			case "download":
				info.Download = val
			case "total":
				info.Total = val
			case "expire":
				info.Expire = val
			}
		}
	}
	return rawProxies, info, nil
}

