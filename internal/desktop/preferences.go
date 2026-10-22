package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func preferencesPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "devin-usage-metrics", "config.json")
}

func language() string {
	var cfg map[string]any
	b, _ := os.ReadFile(preferencesPath())
	_ = json.Unmarshal(b, &cfg)
	if lang, _ := cfg["lang"].(string); lang == "zh" || lang == "en" {
		return lang
	}
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(name); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "zh") {
				return "zh"
			}
			return "en"
		}
	}
	return "en"
}

func saveLanguage(lang string) error { return savePreferences(map[string]any{"lang": lang}) }

// updatePrefs mirrors the update fields Rust keeps in the same config.json.
type updatePrefs struct {
	AutoCheck   bool
	Skipped     string
	LastCheckAt int64
}

func loadUpdatePrefs() updatePrefs {
	var cfg struct {
		AutoCheck   *bool   `json:"auto_check_updates"`
		Skipped     *string `json:"skipped_update_version"`
		LastCheckAt *int64  `json:"last_update_check_at"`
	}
	b, _ := os.ReadFile(preferencesPath())
	_ = json.Unmarshal(b, &cfg)
	p := updatePrefs{AutoCheck: true}
	if cfg.AutoCheck != nil {
		p.AutoCheck = *cfg.AutoCheck
	}
	if cfg.Skipped != nil {
		p.Skipped = *cfg.Skipped
	}
	if cfg.LastCheckAt != nil {
		p.LastCheckAt = *cfg.LastCheckAt
	}
	return p
}

func saveUpdatePrefs(p updatePrefs) error {
	var skipped, last any
	if p.Skipped != "" {
		skipped = p.Skipped
	}
	if p.LastCheckAt != 0 {
		last = p.LastCheckAt
	}
	return savePreferences(map[string]any{"auto_check_updates": p.AutoCheck, "skipped_update_version": skipped, "last_update_check_at": last})
}

// savePreferences merges fields into config.json, keeping keys it does not
// know, and replaces the file atomically.
func savePreferences(fields map[string]any) error {
	path := preferencesPath()
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	for k, v := range fields {
		cfg[k] = v
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
