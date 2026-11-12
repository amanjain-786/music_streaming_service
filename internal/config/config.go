package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	MediaRoots    []string `json:"media_roots"`
	DatabasePath  string   `json:"database_path"`
	Credentials   string   `json:"credentials_file"`
	ListenAddress string   `json:"listen_address"`
}

func Load(path string) (Config, error) {
	absoluteConfigPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}
	file, err := os.Open(absoluteConfigPath)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", absoluteConfigPath, err)
	}
	defer file.Close()

	var cfg Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", absoluteConfigPath, err)
	}
	baseDir := filepath.Dir(absoluteConfigPath)

	if len(cfg.MediaRoots) == 0 {
		return Config{}, fmt.Errorf("media_roots must contain at least one folder")
	}
	for i, root := range cfg.MediaRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			return Config{}, fmt.Errorf("media_roots[%d] is empty", i)
		}
		cfg.MediaRoots[i], err = resolvePath(baseDir, root)
		if err != nil {
			return Config{}, fmt.Errorf("resolve media_roots[%d]: %w", i, err)
		}
	}

	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "plays.db"
	}
	cfg.DatabasePath, err = resolvePath(baseDir, cfg.DatabasePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve database_path: %w", err)
	}

	if cfg.Credentials == "" {
		cfg.Credentials = "users.txt"
	}
	cfg.Credentials, err = resolvePath(baseDir, cfg.Credentials)
	if err != nil {
		return Config{}, fmt.Errorf("resolve credentials_file: %w", err)
	}

	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":8080"
	}
	return cfg, nil
}

func resolvePath(baseDir, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	return filepath.Abs(filepath.Clean(path))
}
