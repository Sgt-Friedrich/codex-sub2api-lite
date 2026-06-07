package main

import (
	"archive/zip"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultListen     = "127.0.0.1:8787"
	defaultUpstream   = "https://chatgpt.com/backend-api/codex/responses"
	defaultRefreshSec = 30
)

type account struct {
	ID               string
	Name             string
	Source           string
	AccessToken      string
	RefreshToken     string
	ChatGPTAccountID string
	ExpiresAt        int64
	Models           []string
	DisabledUntil    time.Time
	LastError         string
}

type accountStore struct {
	dir      string
	mu       sync.RWMutex
	accounts []account
	next     atomic.Uint64
}

type sub2APIFile struct {
	Accounts []struct {
		Name        string                 `json:"name"`
		Platform    string                 `json:"platform"`
		Type        string                 `json:"type"`
		Credentials map[string]any         `json:"credentials"`
		Extra       map[string]any         `json:"extra"`
		Concurrency int                    `json:"concurrency"`
		Priority    int                    `json:"priority"`
		GroupIDs    []int64                `json:"group_ids"`
		ModelMap    map[string]string      `json:"model_mapping"`
		Raw         map[string]interface{} `json:"-"`
	} `json:"accounts"`
}

func main() {
	var listen, dir, upstream, apiKey string
	var refresh int
	flag.StringVar(&listen, "listen", env("CS2API_LISTEN", defaultListen), "listen address")
	flag.StringVar(&dir, "accounts-dir", env("CS2API_ACCOUNTS_DIR", "./accounts"), "directory containing account ZIP or JSON files")
	flag.StringVar(&upstream, "upstream", env("CS2API_UPSTREAM", defaultUpstream), "ChatGPT Codex upstream URL")
	flag.StringVar(&apiKey, "api-key", env("CS2API_API_KEY", ""), "optional client API key")
	flag.IntVar(&refresh, "refresh-seconds", envInt("CS2API_REFRESH_SECONDS", defaultRefreshSec), "account directory refresh interval")
	flag.Parse()

	store := &accountStore{dir: dir}
	if err := store.reload(); err != nil {
		log.Printf("initial account load failed: %v", err)
	}
	go func() {
		t := time.NewTicker(time.Duration(refresh) * time.Second)
		defer t.Stop()
		for range t.C {
			if err := store.reload(); err != nil {
				log.Printf("account reload failed: %v", err)
			}
		}
	}()

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   20 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 0,
			ForceAttemptHTTP2:     true,
		},
	}

	app := &server{store: store, upstream: strings.TrimRight(upstream, "/"), apiKey: apiKey, client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.health)
	mux.HandleFunc("/accounts", app.accounts)
	mux.HandleFunc("/v1/models", app.models)
	mux.HandleFunc("/models", app.models)
	mux.HandleFunc("/v1/responses", app.proxyResponses)
	mux.HandleFunc("/responses", app.proxyResponses)
	mux.HandleFunc("/backend-api/codex/responses", app.proxyResponses)
	mux.HandleFunc("/v1/responses/compact", app.proxyResponses)
	mux.HandleFunc("/responses/compact", app.proxyResponses)
	mux.HandleFunc("/backend-api/codex/responses/compact", app.proxyResponses)

	log.Printf("codex-sub2api-lite listening on %s, accounts=%d, dir=%s", listen, store.count(), dir)
	log.Fatal(http.ListenAndServe(listen, mux))
}

type server struct {
	store    *accountStore
	upstream string
	apiKey   string
	client   *http.Client
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"account_count": s.store.count(),
		"rss_bytes":     currentRSS(),
	})
}

func (s *server) accounts(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	items := s.store.safeAccounts()
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	models := s.store.models()
	if len(models) == 0 {
		models = []string{"gpt-5.4", "gpt-5.3-codex", "gpt-5.2"}
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": 0, "owned_by": "codex-sub2api-lite"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) proxyResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	body = normalizeResponsesBody(body, isCompact(r.URL.Path))

	var lastErr string
	for attempt := 0; attempt < s.store.count(); attempt++ {
		acc, ok := s.store.pick()
		if !ok {
			break
		}
		status, retryable, err := s.forward(w, r, acc, body)
		if err == nil {
			return
		}
		lastErr = err.Error()
		if retryable {
			s.store.markFailure(acc.ID, lastErr, status)
			continue
		}
		http.Error(w, lastErr, status)
		return
	}
	if lastErr == "" {
		lastErr = "no available accounts"
	}
	http.Error(w, lastErr, http.StatusServiceUnavailable)
}

func (s *server) forward(w http.ResponseWriter, inbound *http.Request, acc account, body []byte) (int, bool, error) {
	url := s.upstream
	if isCompact(inbound.URL.Path) && !strings.HasSuffix(url, "/compact") {
		url += "/compact"
	}
	ctx := inbound.Context()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, false, err
	}
	copyForwardHeaders(req.Header, inbound.Header)
	req.Host = "chatgpt.com"
	req.Header.Set("Host", "chatgpt.com")
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Originator", "codex_cli_rs")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "codex_cli_rs/0.128.0")
	}
	if acc.ChatGPTAccountID != "" {
		req.Header.Set("chatgpt-account-id", acc.ChatGPTAccountID)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return http.StatusBadGateway, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, true, fmt.Errorf("account %s upstream %d: %s", acc.ID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		return resp.StatusCode, false, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	for k, vals := range resp.Header {
		if shouldSkipResponseHeader(k) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return resp.StatusCode, false, err
}

func (s *server) authorized(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1
}

func (st *accountStore) reload() error {
	loaded, err := loadAccounts(st.dir)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	old := map[string]account{}
	for _, a := range st.accounts {
		old[a.ID] = a
	}
	for i := range loaded {
		if prev, ok := old[loaded[i].ID]; ok {
			loaded[i].DisabledUntil = prev.DisabledUntil
			loaded[i].LastError = prev.LastError
		}
	}
	st.accounts = loaded
	return nil
}

func (st *accountStore) count() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.accounts)
}

func (st *accountStore) pick() (account, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	now := time.Now()
	n := len(st.accounts)
	if n == 0 {
		return account{}, false
	}
	start := int(st.next.Add(1) % uint64(n))
	for i := 0; i < n; i++ {
		a := st.accounts[(start+i)%n]
		if a.AccessToken == "" || (a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix()) || now.Before(a.DisabledUntil) {
			continue
		}
		return a, true
	}
	return account{}, false
}

func (st *accountStore) markFailure(id, msg string, status int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	backoff := 30 * time.Second
	if status == http.StatusUnauthorized {
		backoff = 10 * time.Minute
	} else if status == http.StatusTooManyRequests {
		backoff = 2 * time.Minute
	}
	for i := range st.accounts {
		if st.accounts[i].ID == id {
			st.accounts[i].LastError = msg
			st.accounts[i].DisabledUntil = time.Now().Add(backoff)
			return
		}
	}
}

func (st *accountStore) safeAccounts() []map[string]any {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]map[string]any, 0, len(st.accounts))
	for _, a := range st.accounts {
		out = append(out, map[string]any{
			"id":             a.ID,
			"name":           redact(a.Name),
			"source":         filepath.Base(a.Source),
			"expires_at":     a.ExpiresAt,
			"models":         a.Models,
			"disabled_until": a.DisabledUntil,
			"last_error":     a.LastError,
		})
	}
	return out
}

func (st *accountStore) models() []string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	seen := map[string]bool{}
	for _, a := range st.accounts {
		for _, m := range a.Models {
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func loadAccounts(dir string) ([]account, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []account
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".zip":
			items, err := loadZip(path)
			if err != nil {
				log.Printf("skip %s: %v", path, err)
				continue
			}
			out = append(out, items...)
		case ".json":
			if !strings.HasPrefix(e.Name(), "sub2api_") {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				log.Printf("skip %s: %v", path, err)
				continue
			}
			out = append(out, parseSub2API(raw, path, nil)...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func loadZip(path string) ([]account, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var subRaw, tokenRaw []byte
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.HasPrefix(filepath.Base(name), "sub2api_") && strings.HasSuffix(name, ".json") {
			subRaw, _ = readZipFile(f)
		}
		if strings.HasPrefix(name, "cpa/token_") && strings.HasSuffix(name, ".json") {
			tokenRaw, _ = readZipFile(f)
		}
	}
	return parseSub2API(subRaw, path, tokenRaw), nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func parseSub2API(raw []byte, source string, tokenRaw []byte) []account {
	if len(raw) == 0 {
		return nil
	}
	var doc sub2APIFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("parse %s failed: %v", source, err)
		return nil
	}
	tokenDoc := map[string]any{}
	if len(tokenRaw) > 0 {
		_ = json.Unmarshal(tokenRaw, &tokenDoc)
	}
	var out []account
	for i, item := range doc.Accounts {
		if strings.ToLower(item.Platform) != "openai" || strings.ToLower(item.Type) != "oauth" {
			continue
		}
		cred := item.Credentials
		if cred == nil {
			cred = map[string]any{}
		}
		access := firstString(cred["access_token"], tokenDoc["access_token"])
		refresh := firstString(cred["refresh_token"], tokenDoc["refresh_token"])
		models := modelsFrom(cred["model_mapping"], item.ModelMap)
		expires := int64From(firstAny(cred["expires_at"], tokenDoc["expires_at"], tokenDoc["expired"]))
		if expires == 0 {
			expires = jwtExp(access)
		}
		id := fmt.Sprintf("%s:%d", filepath.Base(source), i)
		out = append(out, account{
			ID:               id,
			Name:             item.Name,
			Source:           source,
			AccessToken:      access,
			RefreshToken:     refresh,
			ChatGPTAccountID: firstString(cred["chatgpt_account_id"], tokenDoc["account_id"], tokenDoc["chatgpt_account_id"]),
			ExpiresAt:        expires,
			Models:           models,
		})
	}
	return out
}

func normalizeResponsesBody(raw []byte, compact bool) []byte {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}
	if compact {
		delete(body, "store")
		delete(body, "stream")
	} else {
		body["store"] = false
		body["stream"] = true
	}
	if _, ok := body["instructions"]; !ok {
		body["instructions"] = "You are Codex, a concise coding assistant."
	}
	for _, k := range []string{"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "frequency_penalty", "presence_penalty", "user", "metadata", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		delete(body, k)
	}
	if reasoning, ok := body["reasoning"]; ok && reasoning != nil {
		include, _ := body["include"].([]any)
		found := false
		for _, v := range include {
			if s, _ := v.(string); s == "reasoning.encrypted_content" {
				found = true
			}
		}
		if !found {
			body["include"] = append(include, "reasoning.encrypted_content")
		}
	}
	if out, err := json.Marshal(body); err == nil {
		return out
	}
	return raw
}

func copyForwardHeaders(dst, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "host" || lk == "content-length" || lk == "connection" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func shouldSkipResponseHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "content-length", "transfer-encoding", "content-encoding":
		return true
	default:
		return false
	}
}

func isCompact(path string) bool {
	return strings.Contains(strings.TrimRight(path, "/"), "/compact")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func envInt(k string, fallback int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

func firstAny(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func int64From(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		var n int64
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			return n
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func modelsFrom(values ...any) []string {
	seen := map[string]bool{}
	for _, v := range values {
		switch x := v.(type) {
		case map[string]string:
			for k, val := range x {
				if k != "" {
					seen[k] = true
				}
				if val != "" {
					seen[val] = true
				}
			}
		case map[string]any:
			for k, val := range x {
				if k != "" {
					seen[k] = true
				}
				if s, ok := val.(string); ok && s != "" {
					seen[s] = true
				}
			}
		case []any:
			for _, it := range x {
				if s, ok := it.(string); ok && s != "" {
					seen[s] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	return int64From(payload["exp"])
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "@") {
		parts := strings.SplitN(s, "@", 2)
		if len(parts[0]) <= 2 {
			return "***@" + parts[1]
		}
		return parts[0][:2] + "***@" + parts[1]
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func currentRSS() uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	var size, resident uint64
	_, _ = fmt.Fscanf(bytes.NewReader(raw), "%d %d", &size, &resident)
	return resident * uint64(os.Getpagesize())
}
