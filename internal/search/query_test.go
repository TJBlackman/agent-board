package search

import "testing"

func TestParseCompile(t *testing.T) {
	terms, err := Parse(`volume "permission denied" s3-client`)
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 3 || !terms[1].Phrase {
		t.Fatalf("unexpected terms %+v", terms)
	}
	got := Compile(terms)
	want := `"volume" AND "permission denied" AND "s3-client"`
	if got != want {
		t.Fatalf("compile: got %s want %s", got, want)
	}
	// Operators are neutralized by quoting.
	terms, err = Parse(`NOT title:x OR (a)`)
	if err != nil {
		t.Fatal(err)
	}
	got = Compile(terms)
	if got != `"NOT" AND "title:x" AND "OR" AND "(a)"` {
		t.Fatalf("operators not neutralized: %s", got)
	}
	for _, bad := range []string{``, `   `, `"unterminated`, `""`, `- - -`} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExcerpt(t *testing.T) {
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega"
	s, cut := Excerpt(text, []string{"sigma"}, 40)
	if !cut || len([]rune(s)) > 40 || !contains(s, "sigma") {
		t.Fatalf("excerpt %q cut=%v", s, cut)
	}
	s, cut = Excerpt("short text", []string{"zzz"}, 40)
	if cut || s != "short text" {
		t.Fatalf("short excerpt %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
