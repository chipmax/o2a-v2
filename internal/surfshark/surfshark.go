// Package surfshark provisions Surfshark WireGuard tunnels and exposes them to
// the gateway as local SOCKS proxies. All generated state stays inside the
// gateway config directory: a sing-box config, a state file and the proxy file.
package surfshark

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	apiBase   = "https://api.surfshark.com/v1"
	userAgent = "Surfshark/2.24.0 (com.surfshark.vpnclient.ios; build:19; iOS 14.8.1) Alamofire/5.4.3 device/mobile"
)

// Config controls where state is written and how tunnels are run.
type Config struct {
	ConfigDir    string
	ProxyFile    string
	BinaryPath   string
	BasePort     int
	Logger       *slog.Logger
	ReloadConfig func() error
}

// Manager owns the sing-box child process and the generated proxy entries.
type Manager struct {
	cfg   Config
	mu    sync.Mutex
	cmd   *exec.Cmd
	done  chan struct{}
	state State
}

// State is both the persisted state file and the status response.
type State struct {
	Running    bool     "json:\"running\""
	Tunnels    int      "json:\"tunnels\""
	ConfigPath string   "json:\"config_path\""
	Ports      []int    "json:\"ports\""
	Countries  []string "json:\"countries\""
	StartedAt  string   "json:\"started_at,omitempty\""
	LastError  string   "json:\"last_error,omitempty\""
}

// SetupRequest is the Surfshark form. PrivateKey is optional.
type SetupRequest struct {
	Username   string   "json:\"username\""
	Password   string   "json:\"password\""
	PrivateKey string   "json:\"private_key\""
	Count      int      "json:\"count\""
	Countries  []string "json:\"countries\""
	BasePort   int      "json:\"base_port\""
}

type server struct {
	CountryCode    string "json:\"countryCode\""
	ConnectionName string "json:\"connectionName\""
	PubKey         string "json:\"pubKey\""
	Load           int    "json:\"load\""
}

type loginResponse struct {
	Token   string "json:\"token\""
	Code    int    "json:\"code\""
	Message string "json:\"message\""
}

// New loads saved state and restarts a previously configured tunnel set.
func New(cfg Config) *Manager {
	if cfg.BasePort <= 0 {
		cfg.BasePort = 13101
	}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "/usr/local/bin/sing-box"
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = "."
	}
	if cfg.ProxyFile == "" {
		cfg.ProxyFile = filepath.Join(cfg.ConfigDir, "proxyfile")
	}
	m := &Manager{cfg: cfg}
	m.loadState()
	if len(m.state.Ports) > 0 && m.configExists() {
		if err := m.start(); err != nil {
			m.state.LastError = err.Error()
			m.logger().Warn("surfshark tunnel restart failed", "component", "surfshark", "event", "tunnel_restart_failed", "error", err)
		}
	}
	return m
}

func (m *Manager) logger() *slog.Logger {
	if m.cfg.Logger != nil {
		return m.cfg.Logger
	}
	return slog.Default()
}
func (m *Manager) configPath() string { return filepath.Join(m.cfg.ConfigDir, "surfshark.json") }
func (m *Manager) statePath() string  { return filepath.Join(m.cfg.ConfigDir, "surfshark-state.json") }
func (m *Manager) configExists() bool { _, err := os.Stat(m.configPath()); return err == nil }

func (m *Manager) loadState() {
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var s State
	if err := json.Unmarshal(raw, &s); err == nil {
		m.state = s
	}
}

func (m *Manager) saveState() {
	m.state.Running = m.cmd != nil && m.cmd.ProcessState == nil
	raw, err := json.MarshalIndent(m.state, "", "  ")
	if err == nil {
		_ = os.WriteFile(m.statePath(), raw, 0o600)
	}
}

// Status answers GET /api/surfshark/status.
func (m *Manager) Status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	state.Running = m.cmd != nil && m.cmd.ProcessState == nil
	return state
}

func apiRequest(ctx context.Context, method, path, token string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=utf-8")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if len(message) > 300 {
			message = message[:300]
		}
		return nil, fmt.Errorf("surfshark api %s: HTTP %d: %s", path, resp.StatusCode, message)
	}
	return raw, nil
}

func login(ctx context.Context, username, password string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	raw, err := apiRequest(ctx, http.MethodPost, "/auth/login", "", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	var response loginResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", fmt.Errorf("surfshark login rejected: %s", response.Message)
	}
	return response.Token, nil
}

func fetchServers(ctx context.Context, token string) ([]server, error) {
	raw, err := apiRequest(ctx, http.MethodGet, "/servers", token, nil)
	if err != nil {
		return nil, err
	}
	var servers []server
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

func generateKey(ctx context.Context, token string) (string, error) {
	raw, err := apiRequest(ctx, http.MethodGet, "/wireguard/generate", token, nil)
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	for _, key := range []string{"privateKey", "private_key", "privatekey"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value, nil
		}
	}
	return "", errors.New("surfshark key generation returned no private key")
}

func resolveHost(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		return ""
	}
	for _, ip := range addresses {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

type singBoxConfig struct {
	Log       map[string]any   "json:\"log\""
	Inbounds  []map[string]any "json:\"inbounds\""
	Endpoints []map[string]any "json:\"endpoints\""
	Outbounds []map[string]any "json:\"outbounds\""
	Route     map[string]any   "json:\"route\""
}

// Setup provisions N tunnels and exposes them as local SOCKS proxies.
func (m *Manager) Setup(ctx context.Context, req SetupRequest) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Count < 1 || req.Count > 50 {
		return State{}, errors.New("count must be between 1 and 50")
	}
	base := req.BasePort
	if base <= 0 {
		base = m.cfg.BasePort
	}
	if base < 1024 || base+req.Count > 65535 {
		return State{}, errors.New("invalid port range")
	}
	privateKey := strings.TrimSpace(req.PrivateKey)
	allowed := map[string]bool{}
	for _, code := range req.Countries {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code != "" {
			allowed[code] = true
		}
	}
	token := ""
	var tokenErr error
	if privateKey == "" {
		token, tokenErr = login(ctx, strings.TrimSpace(req.Username), req.Password)
		if tokenErr != nil {
			return State{}, tokenErr
		}
	}
	servers, err := fetchServers(ctx, token)
	if err != nil {
		return State{}, err
	}
	candidates := make([]server, 0, len(servers))
	for _, s := range servers {
		s.CountryCode = strings.ToUpper(strings.TrimSpace(s.CountryCode))
		if len(allowed) > 0 && !allowed[s.CountryCode] {
			continue
		}
		if s.ConnectionName == "" || s.PubKey == "" {
			continue
		}
		candidates = append(candidates, s)
	}
	if len(candidates) < req.Count {
		return State{}, fmt.Errorf("only %d servers match the selected countries; need %d", len(candidates), req.Count)
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].Load < candidates[i].Load {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	picked := candidates[:req.Count]
	if privateKey == "" {
		privateKey, err = generateKey(ctx, token)
		if err != nil {
			return State{}, err
		}
	}
	cfg := singBoxConfig{
		Log:      map[string]any{"level": "warn", "timestamp": true},
		Inbounds: []map[string]any{}, Endpoints: []map[string]any{},
		Outbounds: []map[string]any{{"type": "direct", "tag": "direct"}},
		Route:     map[string]any{"rules": []map[string]any{}, "final": "direct"},
	}
	rules := []map[string]any{}
	ports := []int{}
	codes := []string{}
	for i, s := range picked {
		ip := resolveHost(s.ConnectionName)
		if ip == "" {
			return State{}, fmt.Errorf("cannot resolve %s", s.ConnectionName)
		}
		tag := fmt.Sprintf("ss-%s-%d", strings.ToLower(s.CountryCode), i)
		inbound := "in-" + tag
		port := base + i
		cfg.Inbounds = append(cfg.Inbounds, map[string]any{"type": "socks", "tag": inbound, "listen": "127.0.0.1", "listen_port": port})
		cfg.Endpoints = append(cfg.Endpoints, map[string]any{
			"type": "wireguard", "tag": tag,
			"address":     []string{"10.14.0.2/16", "fd00::2/64"},
			"private_key": privateKey, "mtu": 1420,
			"peers": []map[string]any{{
				"address": ip, "port": 51820, "public_key": s.PubKey,
				"allowed_ips":                   []string{"0.0.0.0/0", "::/0"},
				"persistent_keepalive_interval": 25,
			}},
		})
		rules = append(rules, map[string]any{"inbound": []string{inbound}, "outbound": tag})
		ports = append(ports, port)
		codes = append(codes, s.CountryCode)
	}
	cfg.Route["rules"] = rules
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return State{}, err
	}
	if err := m.stopLocked(); err != nil {
		return State{}, err
	}
	path := m.configPath()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return State{}, fmt.Errorf("write sing-box config: %w", err)
	}
	if err := m.startLocked(path); err != nil {
		_ = os.Remove(path)
		return State{}, err
	}
	m.state = State{Running: true, Tunnels: len(picked), ConfigPath: path, Ports: ports, Countries: codes, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := m.updateProxyFileLocked(ports); err != nil {
		return m.state, err
	}
	if m.cfg.ReloadConfig != nil {
		if err := m.cfg.ReloadConfig(); err != nil {
			return m.state, fmt.Errorf("tunnels started but gateway reload failed: %w", err)
		}
	}
	m.saveState()
	return m.state, nil
}

func (m *Manager) startLocked(path string) error {
	if _, err := os.Stat(m.cfg.BinaryPath); err != nil {
		return fmt.Errorf("sing-box binary %s unavailable: %w", m.cfg.BinaryPath, err)
	}
	cmd := exec.Command(m.cfg.BinaryPath, "run", "-c", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	done := make(chan struct{})
	m.cmd = cmd
	m.done = done
	if err := cmd.Start(); err != nil {
		m.cmd = nil
		m.done = nil
		_ = writer.Close()
		_ = reader.Close()
		return err
	}
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			m.logger().Info("sing-box", "component", "surfshark", "event", "tunnel_log", "line", scanner.Text())
		}
	}()
	go func() { _ = cmd.Wait(); _ = writer.Close() }()
	select {
	case <-done:
		return errors.New("sing-box exited during startup; check credentials and server list")
	case <-time.After(2 * time.Second):
	}
	if cmd.ProcessState != nil {
		return errors.New("sing-box exited during startup")
	}
	return nil
}

func (m *Manager) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(m.configPath())
}

func (m *Manager) stopLocked() error {
	if m.cmd == nil {
		return nil
	}
	if m.cmd.Process != nil {
		_ = syscall.Kill(-m.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-m.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	m.cmd = nil
	m.done = nil
	return nil
}

// Stop kills the tunnels and removes their proxy entries.
func (m *Manager) Stop() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stopLocked(); err != nil {
		return m.state, err
	}
	m.state.Running = false
	hadPorts := len(m.state.Ports) > 0
	m.state.Ports = nil
	m.state.Tunnels = 0
	if hadPorts {
		if err := m.updateProxyFileLocked(nil); err != nil {
			return m.state, err
		}
	}
	if m.cfg.ReloadConfig != nil {
		if err := m.cfg.ReloadConfig(); err != nil {
			return m.state, err
		}
	}
	m.saveState()
	return m.state, nil
}

func (m *Manager) updateProxyFileLocked(ports []int) error {
	path := m.cfg.ProxyFile
	lines := []string{}
	if raw, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if managedPort(trimmed, m.cfg.BasePort) {
				continue
			}
			lines = append(lines, trimmed)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, port := range ports {
		lines = append(lines, "socks5h://127.0.0.1:"+strconv.Itoa(port))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func managedPort(entry string, base int) bool {
	if base <= 0 {
		return false
	}
	const prefix = "socks5h://127.0.0.1:"
	if !strings.HasPrefix(entry, prefix) {
		return false
	}
	port, err := strconv.Atoi(strings.TrimPrefix(entry, prefix))
	if err != nil {
		return false
	}
	return port >= base && port < base+100
}
