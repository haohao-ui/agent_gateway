package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

type NetworkConfig struct {
	EnableHTTP bool `json:"enable_http"`
	HTTPPort   int  `json:"http_port,omitempty"`
}

func (s *Server) networkConfigPath() string {
	if s.dataDir == "" {
		return "./gateway-data/network.json"
	}
	return filepath.Join(s.dataDir, "network.json")
}

func (s *Server) LoadNetworkConfig() NetworkConfig {
	cfg := NetworkConfig{
		EnableHTTP: false,
		HTTPPort:   8080,
	}
	data, err := os.ReadFile(s.networkConfigPath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.HTTPPort <= 0 {
		cfg.HTTPPort = 8080
	}
	return cfg
}

func (s *Server) SaveNetworkConfig(cfg NetworkConfig) error {
	dir := filepath.Dir(s.networkConfigPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if cfg.HTTPPort <= 0 {
		cfg.HTTPPort = 8080
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.networkConfigPath(), data, 0o600)
}

func (s *Server) handleGetNetworkSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.LoadNetworkConfig()
	currentMode := "https"
	if r.TLS == nil {
		currentMode = "http"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enable_http":  cfg.EnableHTTP,
		"http_port":    cfg.HTTPPort,
		"current_mode": currentMode,
	})
}

func (s *Server) handleUpdateNetworkSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPrincipal(w, r) {
		return
	}
	var in struct {
		EnableHTTP bool `json:"enable_http"`
		HTTPPort   int  `json:"http_port"`
	}
	if !readJSON(w, r, 4096, &in) {
		return
	}
	cfg := NetworkConfig{
		EnableHTTP: in.EnableHTTP,
		HTTPPort:   in.HTTPPort,
	}
	if err := s.SaveNetworkConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save network configuration: "+err.Error())
		return
	}
	s.SetHTTPAllowed(cfg.EnableHTTP)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"enable_http": cfg.EnableHTTP,
		"http_port":   cfg.HTTPPort,
		"message":     "网络配置已保存，重启网关服务后生效",
	})
}
