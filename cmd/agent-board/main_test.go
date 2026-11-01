package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidWebConfigurationDoesNotCreateDatabase(t *testing.T) {
	data := filepath.Join(t.TempDir(), "uncreated")
	t.Setenv("AGENT_BOARD_DATA_DIR", data)
	t.Setenv("AGENT_BOARD_LISTEN", "127.0.0.1:18089")
	t.Setenv("AGENT_BOARD_WEB_PRINCIPAL_ID", "invalid")
	t.Setenv("AGENT_BOARD_WEB_ORIGIN", "https://board.example.com")
	t.Setenv("AGENT_BOARD_WEB_ALLOW_HTTP", "")
	if err := serve(); err == nil {
		t.Fatal("invalid settings were accepted")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("startup touched the database directory: %v", err)
	}
}

func TestHealthAddr(t *testing.T) {
	cases := map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		"[::]:8080":      "127.0.0.1:8080",
		"127.0.0.1:9090": "127.0.0.1:9090",
		"[::1]:8080":     "[::1]:8080",
		"localhost:8080": "localhost:8080",
		"not-an-address": "not-an-address",
	}
	for in, want := range cases {
		if got := healthAddr(in); got != want {
			t.Errorf("healthAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
