package wasmguest

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeValueTable(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "null"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"int", int64(42), "42"},
		{"int min", int64(math.MinInt64), "-9223372036854775808"},
		{"int max", int64(math.MaxInt64), "9223372036854775807"},
		{"float one", float64(1), "1.0"},
		{"float frac", 1.5, "1.5"},
		{"float exp", 1e20, "1e+20"},
		{"string", "héllo\n\"\\", `"héllo\n\"\\"`},
		{"array", []any{int64(1), float64(2), "x", nil, true}, `[1,2.0,"x",null,true]`},
		{"map", map[string]any{"b": int64(2), "a": int64(1)}, `{"a":1,"b":2}`},
		{"nil slice", []any(nil), `[]`},
		{"nil map", map[string]any(nil), `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeValue(tc.in)
			if err != nil {
				t.Fatalf("encodeValue: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("encodeValue = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDecodeValueTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"nil", "null", nil},
		{"true", "true", true},
		{"int", "1", int64(1)},
		{"float", "1.0", float64(1)},
		{"float exp", "1e2", float64(100)},
		{"string", `"x"`, "x"},
		{"array", `[1,2.0,"x",null,true]`, []any{int64(1), float64(2), "x", nil, true}},
		{"map", `{"a":1,"b":2.0}`, map[string]any{"a": int64(1), "b": float64(2)}},
		{"surrogate pair", `"\ud83d\ude00"`, "\U0001F600"},
		{"escaped backslash before u", `"\\uD800"`, `\uD800`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeValue([]byte(tc.in))
			if err != nil {
				t.Fatalf("decodeValue: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeValue = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEncodeValueRejects(t *testing.T) {
	cycleSlice := make([]any, 1)
	cycleSlice[0] = cycleSlice
	cycleMap := map[string]any{}
	cycleMap["self"] = cycleMap

	cases := []struct {
		name string
		in   any
	}{
		{"int", int(1)},
		{"uint64", uint64(1)},
		{"float32", float32(1)},
		{"nan", math.NaN()},
		{"inf", math.Inf(1)},
		{"bytes", []byte{1, 2, 3}},
		{"byte array", [3]byte{1, 2, 3}},
		{"chan", make(chan int)},
		{"func", func() {}},
		{"struct", struct{ A int }{1}},
		{"non-string map", map[int]any{1: "x"}},
		{"invalid utf8", string([]byte{0xff})},
		{"cycle slice", cycleSlice},
		{"cycle map", cycleMap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodeValue(tc.in); err == nil {
				t.Fatalf("expected error for %T", tc.in)
			}
		})
	}
}

func TestEncodeValueDepth(t *testing.T) {
	var v any = int64(1)
	for i := 0; i < maxValueDepth+1; i++ {
		v = []any{v}
	}
	if _, err := encodeValue(v); err == nil {
		t.Fatal("expected depth error")
	}
}

func TestEncodeValueNodeCount(t *testing.T) {
	v := make([]any, maxValueNodes+1)
	if _, err := encodeValue(v); err == nil {
		t.Fatal("expected node count error")
	}
}

func TestDecodeValueRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"out of range int", "9223372036854775808"},
		{"non-finite float", "1e400"},
		{"unpaired high surrogate", `"\ud800"`},
		{"unpaired low surrogate", `"\udc00"`},
		{"duplicate key", `{"a":1,"a":2}`},
		{"trailing", `1 2`},
		{"invalid utf8", string([]byte{'"', 0xff, '"'})},
		{"deep", strings.Repeat("[", maxValueDepth+2) + strings.Repeat("]", maxValueDepth+2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeValue([]byte(tc.in)); err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
		})
	}
}

func TestValueRoundTrip(t *testing.T) {
	values := []any{
		nil,
		true,
		int64(-7),
		float64(1),
		float64(1.5),
		"héllo\n\"\\",
		[]any{int64(1), float64(2), "x", nil, true},
		map[string]any{"a": int64(1), "b": []any{true, float64(1), "x"}, "c": nil},
	}
	for _, v := range values {
		enc, err := encodeValue(v)
		if err != nil {
			t.Fatalf("encodeValue(%#v): %v", v, err)
		}
		got, err := decodeValue(enc)
		if err != nil {
			t.Fatalf("decodeValue(%s): %v", enc, err)
		}
		if !reflect.DeepEqual(got, v) {
			t.Fatalf("roundtrip %#v -> %s -> %#v", v, enc, got)
		}
	}
}

func FuzzValueRoundTrip(f *testing.F) {
	seeds := []string{
		`null`,
		`true`,
		`1`,
		`1.0`,
		`"x"`,
		`[1,2.0,"x",null,true]`,
		`{"a":1,"b":[true,1.0,"x"]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := decodeValue(data)
		if err != nil {
			return
		}
		enc, err := encodeValue(v)
		if err != nil {
			t.Fatalf("encode decoded value: %v", err)
		}
		v2, err := decodeValue(enc)
		if err != nil {
			t.Fatalf("decode re-encoded value: %v", err)
		}
		if !reflect.DeepEqual(v, v2) {
			t.Fatalf("roundtrip mismatch: %#v != %#v", v, v2)
		}
	})
}
