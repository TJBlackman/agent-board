package pagination

import (
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	p := Payload{Kind: KindFeed, Gen: "g1", Principal: "p1", FP: "fp", Exp: now.Add(time.Hour).Unix(), Watermark: 10, Pos: 5}
	s, err := Encode(secret, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(secret, s, KindFeed, "g1", "p1", "fp", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Watermark != 10 || got.Pos != 5 {
		t.Fatalf("payload mismatch %+v", got)
	}
	if _, err := Decode(secret, s, KindFeed, "g2", "p1", "fp", now); err != ErrGen {
		t.Fatalf("expected generation error, got %v", err)
	}
	if _, err := Decode(secret, s, KindFeed, "g1", "p2", "fp", now); err != ErrMismatch {
		t.Fatalf("expected mismatch, got %v", err)
	}
	if _, err := Decode(secret, s, KindFeed, "g1", "p1", "fp", now.Add(2*time.Hour)); err != ErrExpired {
		t.Fatalf("expected expired, got %v", err)
	}
	tampered := s[:len(s)-2] + "zz"
	if _, err := Decode(secret, tampered, KindFeed, "g1", "p1", "fp", now); err == nil {
		t.Fatal("tampered cursor accepted")
	}
	if _, err := Decode([]byte("other-secret"), s, KindFeed, "g1", "p1", "fp", now); err != ErrSignature {
		t.Fatalf("expected signature error, got %v", err)
	}
}
