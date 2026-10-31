package strictjson

import "testing"

func TestValidateAccepts(t *testing.T) {
	good := []string{
		`{}`,
		`{"a":1,"b":[1,2,{"c":null}]}`,
		`{"s":"😀"}`,
		`{"a":"x y"}`,
		` {"a" : true } `,
		`{"n":-1.5e3}`,
	}
	for _, g := range good {
		if err := Validate([]byte(g), 4); err != nil {
			t.Errorf("%s: unexpected %v", g, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []string{
		`{"a":1,"a":2}`,
		`{"a":1,"a":2}`,
		`{"s":"\ud83d"}`,
		`{"s":"\ude00"}`,
		`{"s":"\ud83dx"}`,
		`[[[[[1]]]]]`,
		`{"a":1} x`,
		"{\"s\":\"\xff\"}",
		`{"a":01}`,
		"{\"s\":\"a\nb\"}",
		`{"a":1,}`,
		`{"a":tru}`,
	}
	for _, b := range bad {
		if err := Validate([]byte(b), 4); err == nil {
			t.Errorf("%q: expected an error", b)
		}
	}
}

func TestDecodeUnknownField(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := Decode([]byte(`{"a":1,"b":2}`), 4, &v); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	if err := Decode([]byte(`{"a":1}`), 4, &v); err != nil || v.A != 1 {
		t.Fatalf("decode failed: %v", err)
	}
}
