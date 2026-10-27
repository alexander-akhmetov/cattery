package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Picker struct {
		Preview *bool `toml:"preview"`
	} `toml:"picker"`
	Session struct {
		File string `toml:"file"`
	} `toml:"session"`
	Resume struct {
		Prefix   string `toml:"prefix"`
		Claude   string `toml:"claude"`
		Codex    string `toml:"codex"`
		Opencode string `toml:"opencode"`
		Pi       string `toml:"pi"`
	} `toml:"resume"`

	dir string
}

// go-toml bypasses UnmarshalText for string kinds, so the validator is a struct.
type prefixCheck struct{}

func (*prefixCheck) UnmarshalText(text []byte) error {
	if strings.ContainsFunc(string(text), unicode.IsControl) {
		return errors.New("resume prefix contains a control character")
	}
	return nil
}

func Load() (Config, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("locate HOME/.config/cattery/config.toml: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	path, err := filepath.Abs(filepath.Join(dir, "cattery", "config.toml"))
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path %q: %w", filepath.Join(dir, "cattery", "config.toml"), err)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var c Config
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&c); err != nil {
		if missing, ok := errors.AsType[*toml.StrictMissingError](err); ok {
			details := make([]string, 0, len(missing.Errors))
			for _, field := range missing.Errors {
				line, column := field.Position()
				details = append(details, fmt.Sprintf("unknown key %q at line %d, column %d", strings.Join(field.Key(), "."), line, column))
			}
			err = errors.New(strings.Join(details, "; "))
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	// Validate each assignment before aliases can overwrite it. The first decode
	// enforces types because go-toml also calls UnmarshalText for non-string scalars.
	var prefixes struct {
		Resume map[string]prefixCheck `toml:"resume"`
	}
	if err := toml.Unmarshal(data, &prefixes); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	c.dir = filepath.Dir(path)
	return c, nil
}

func (c Config) Preview() bool {
	if value := os.Getenv("CATTERY_PREVIEW"); value != "" {
		return value == "1"
	}
	return c.Picker.Preview != nil && *c.Picker.Preview
}

func (c Config) ResumePrefix(kind string) string {
	var env, prefix string
	switch kind {
	case "claude":
		env, prefix = "CATTERY_RESUME_PREFIX_CLAUDE", c.Resume.Claude
	case "codex":
		env, prefix = "CATTERY_RESUME_PREFIX_CODEX", c.Resume.Codex
	case "opencode":
		env, prefix = "CATTERY_RESUME_PREFIX_OPENCODE", c.Resume.Opencode
	case "pi":
		env, prefix = "CATTERY_RESUME_PREFIX_PI", c.Resume.Pi
	}
	if env != "" {
		if value := os.Getenv(env); value != "" {
			return value
		}
	}
	if value := os.Getenv("CATTERY_RESUME_PREFIX"); value != "" {
		return value
	}
	if prefix != "" {
		return prefix
	}
	if c.Resume.Prefix != "" {
		return c.Resume.Prefix
	}
	return kind
}

func (c Config) SessionPath() (string, error) {
	path := os.Getenv("CATTERY_SESSION_FILE")
	if path == "" {
		path = c.Session.File
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve session path %q: %w", path, err)
			}
			path = filepath.Join(home, path[2:])
		} else if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(c.dir, path)
		}
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve default session path: %w", err)
			}
			path = filepath.Join(home, ".local", "share", "kitty", "sessions", "agents.kitty-session")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve session path %q: %w", path, err)
	}
	return abs, nil
}
