package api

import (
	"context"
	"path/filepath"
	"testing"

	"trevorblackman.dev/agent-board/internal/storage"
)

func TestValidateAdminListen(t *testing.T) {
	ok := []string{"", "127.0.0.1:8081", "[::1]:8081", "localhost:8081", "127.0.0.2:1"}
	for _, addr := range ok {
		if err := validateAdminListen(addr); err != nil {
			t.Errorf("%q rejected: %v", addr, err)
		}
	}
	bad := []string{"0.0.0.0:8081", ":8081", "[::]:8081", "10.0.0.5:8081", "board.example.com:8081", "8081"}
	for _, addr := range bad {
		if err := validateAdminListen(addr); err == nil {
			t.Errorf("%q accepted", addr)
		}
	}
}

func TestNewRejectsExposedAdminListener(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "board.db"), storage.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := New(Config{AdminListen: "0.0.0.0:8081"}, db); err == nil {
		t.Fatal("exposed admin listener accepted")
	}
}
