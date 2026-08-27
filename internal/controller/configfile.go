package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/apeters/homebench/internal/protocol"
)

// LoadConfigFile reads a JSON config from path.
func LoadConfigFile(path string) (protocol.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.Config{}, err
	}
	var cfg protocol.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return protocol.Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfigFile writes cfg as indented JSON, creating parent dirs if needed.
func SaveConfigFile(path string, cfg protocol.Config) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
