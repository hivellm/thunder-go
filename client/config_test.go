package client_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivellm/thunder-go/client"
	"gopkg.in/yaml.v3"
)

// TestStandardMatchesConformanceYAML pins Config.Standard() to
// conformance/standard.yaml (PRO-013), so the Go lane can never disagree with
// the other implementations about what "standard" means.
func TestStandardMatchesConformanceYAML(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "conformance", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// This module is mirrored to github.com/hivellm/thunder-go, where the
		// monorepo's conformance/ directory is not present. Skip rather than
		// fail there: the corpus is the source of truth and it is checked in
		// the monorepo, which is where changes to this file land. A consumer
		// who cloned the mirror should not see a red suite for a file that was
		// never theirs.
		t.Skipf("conformance/standard.yaml not reachable (standalone mirror?): %v", err)
	}
	var std struct {
		Handshake     string `yaml:"handshake"`
		HelloStyle    string `yaml:"hello_style"`
		Push          string `yaml:"push"`
		MaxFrameBytes int    `yaml:"max_frame_bytes"`
		MaxInFlight   int    `yaml:"max_in_flight"`
		ErrorCodes    string `yaml:"error_codes"`
		Tls           string `yaml:"tls"`
	}
	if err := yaml.Unmarshal(raw, &std); err != nil {
		t.Fatalf("parse standard.yaml: %v", err)
	}

	c := client.Standard()
	checkStr(t, "handshake", c.Handshake.String(), std.Handshake)
	checkStr(t, "hello_style", c.HelloStyle.String(), std.HelloStyle)
	checkStr(t, "push", c.Push.String(), std.Push)
	if c.MaxFrameBytes != std.MaxFrameBytes {
		t.Fatalf("max_frame_bytes: got %d want %d", c.MaxFrameBytes, std.MaxFrameBytes)
	}
	if c.MaxInFlight != std.MaxInFlight {
		t.Fatalf("max_in_flight: got %d want %d", c.MaxInFlight, std.MaxInFlight)
	}
	checkStr(t, "error_codes", c.ErrorCodes.String(), std.ErrorCodes)
	checkStr(t, "tls", c.Tls.String(), std.Tls)
	// Identity is the application's — the standard supplies neither.
	if c.Scheme != "" || c.DefaultPort != 0 {
		t.Fatalf("standard must carry no identity: scheme=%q port=%d", c.Scheme, c.DefaultPort)
	}
}

func checkStr(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %q want %q", field, got, want)
	}
}
