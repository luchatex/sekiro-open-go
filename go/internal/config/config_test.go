package config

import (
	"log/slog"
	"testing"
)

func TestSlogLevelDefault(t *testing.T) {
	c := &Config{}
	lv, ok := c.SlogLevel()
	if !ok || lv != slog.LevelInfo {
		t.Fatalf("default: want info/ok, got %v/%v", lv, ok)
	}
}

func TestSlogLevelDebugOverridesLogLevel(t *testing.T) {
	// -debug 优先级最高：即使 -log-level 给了别的值也强制 Debug
	c := &Config{LocalDebug: true, LogLevel: "error"}
	lv, ok := c.SlogLevel()
	if !ok || lv != slog.LevelDebug {
		t.Fatalf("debug override: want debug/ok, got %v/%v", lv, ok)
	}
}

func TestSlogLevelValidValues(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"WARN":  slog.LevelWarn,
		"Error": slog.LevelError,
	}
	for in, want := range cases {
		c := &Config{LogLevel: in}
		lv, ok := c.SlogLevel()
		if !ok || lv != want {
			t.Errorf("LogLevel=%q: want %v/ok, got %v/%v", in, want, lv, ok)
		}
	}
}

func TestSlogLevelInvalidFallsBackToInfo(t *testing.T) {
	c := &Config{LogLevel: "verbose"}
	lv, ok := c.SlogLevel()
	if ok || lv != slog.LevelInfo {
		t.Fatalf("invalid: want info/!ok, got %v/%v", lv, ok)
	}
}
