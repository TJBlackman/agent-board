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
