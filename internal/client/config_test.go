package client

import "testing"

func TestCheckBoardURL(t *testing.T) {
	t.Setenv("AGENT_BOARD_ALLOW_HTTP", "")
	ok := []string{"https://board.example.com", "http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"}
	for _, u := range ok {
		if err := checkBoardURL(u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
	bad := []string{"http://board.example.com", "http://10.0.0.5:8080", "board.example.com", "ftp://board.example.com", ""}
	for _, u := range bad {
		if err := checkBoardURL(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	t.Setenv("AGENT_BOARD_ALLOW_HTTP", "true")
	if err := checkBoardURL("http://board.example.com"); err != nil {
		t.Errorf("override ignored: %v", err)
	}
}

func TestSafeFile(t *testing.T) {
	if got := SafeFile("a/b:c d.json"); got != "a_b_c_d.json" {
		t.Errorf("SafeFile = %q", got)
	}
}
