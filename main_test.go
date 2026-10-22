package main

import (
	"encoding/json"
	"regexp"
	"testing"
)

// The embedded mygo.json is the app's one version: packages, the bundle,
// every view and the updater use it, and the updater compares it as x.y.z.
func TestEmbeddedVersionIsRelease(t *testing.T) {
	var config struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(appConfig, &config); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(config.Version) {
		t.Fatalf("mygo.json version %q is not x.y.z", config.Version)
	}
}
