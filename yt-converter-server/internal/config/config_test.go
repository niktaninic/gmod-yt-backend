package config

import (
	"os"
	"path/filepath"
	"testing"
)

func load(t *testing.T, env map[string]string) *Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("API_SECRET", "a-very-long-test-secret")
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPlaceholderSecretRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("CACHE_DIR", dir)
	for _, s := range []string{"", "change-me-to-a-strong-secret"} {
		t.Setenv("API_SECRET", s)
		if _, err := Load(""); err == nil {
			t.Errorf("secret %q accepted", s)
		}
	}
}

func TestZeroValuesKept(t *testing.T) {
	c := load(t, map[string]string{"AUDIO_QUALITY": "0", "MAX_DURATION_SECONDS": "0"})
	if c.AudioQuality != 0 || c.MaxDurationSeconds != 0 {
		t.Fatalf("zero values lost: quality=%d max=%d", c.AudioQuality, c.MaxDurationSeconds)
	}
}

func TestLimitsFor(t *testing.T) {
	c := load(t, map[string]string{"MAX_DURATION_SECONDS": "600"})
	if err := os.WriteFile(c.RankLimitsPath, []byte(`{"user":{"maxDurationSeconds":300,"dailyConversions":5},"vip":{"maxDurationSeconds":0,"dailyConversions":-1},"admin":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.ReloadRankLimits(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Effective{
		"user":    {MaxDurationSeconds: 300, DailyConversions: 5},
		"vip":     {},
		"admin":   {},
		"default": {MaxDurationSeconds: 600}, // unconfigured rank -> global cap (old code: unlimited)
	}
	for rank, want := range cases {
		if got := c.LimitsFor(rank); got != want {
			t.Errorf("%s: got %+v want %+v", rank, got, want)
		}
	}
	if err := c.SaveRankLimits(map[string]RankLimit{"user": {MaxDurationSeconds: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := c.LimitsFor("vip"); got.MaxDurationSeconds != 600 {
		t.Errorf("after save, vip should fall back to global: %+v", got)
	}
}
