package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/config"
)

func TestWriteUsesSpecKeysAndReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := config.Config{HeadscaleURL: "https://headscale.example", DisplayName: "laptop", Hostname: "defios-a1b2c3d4"}
	if err := config.WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`headscale_url = "https://headscale.example"`,
		`display_name = "laptop"`,
		`hostname = "defios-a1b2c3d4"`,
	} {
		if !strings.Contains(string(raw), line) {
			t.Errorf("config.toml missing %q:\n%s", line, raw)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", info.Mode().Perm())
	}
	out, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("read %+v, want %+v", out, in)
	}
}
