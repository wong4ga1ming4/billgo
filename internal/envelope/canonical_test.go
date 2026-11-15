package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalizeSortsAndStrips(t *testing.T) {
	in := map[string]any{
		"b": 2,
		"a": 1,
		"c": map[string]any{"d": true, "e": []any{1, 2}},
	}
	got, err := Canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":1,"b":2,"c":{"d":true,"e":[1,2]}}`
	if string(got) != want {
		t.Fatalf("规范化结果\n got: %s\nwant: %s", got, want)
	}
}

func TestCanonicalizeIgnoresFormatting(t *testing.T) {
	a, err := Canonicalize(json.RawMessage(`{ "x" : 1, "y" : { "b":2, "a":1 } }`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonicalize(json.RawMessage(`{"y":{"a":1,"b":2},"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("同样语义的两种写法规范化后应相同:\n%s\n%s", a, b)
	}
}

func TestCanonicalizeRejectsFloat(t *testing.T) {
	_, err := Canonicalize(json.RawMessage(`{"x":1.5}`))
	if err == nil || !strings.Contains(err.Error(), "整数") {
		t.Fatalf("浮点应该被拒绝，得到 %v", err)
	}
}

func TestCanonicalizeEscapesConsistently(t *testing.T) {
	got, err := Canonicalize(map[string]any{"s": "a\nb\"c"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"s":"a\nb\"c"}` {
		t.Fatalf("转义不对: %s", got)
	}
}
