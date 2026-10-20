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

func saveLanguage(lang string) error {
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
	cfg["lang"] = lang
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
