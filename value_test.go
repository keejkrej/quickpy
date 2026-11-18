package quickpy

import (
	"math"
	"testing"
)

func TestValidateValue(t *testing.T) {
	valid := []Value{
		nil,
		true,
		int64(0),
		int64(math.MaxInt64),
		int64(math.MinInt64),
		1.5,
		"x",
		[]any{int64(1), "x", nil},
		map[string]any{"a": int64(1), "b": []any{true}},
	}
	for _, v := range valid {
		if err := validateValue(v, 0); err != nil {
			t.Fatalf("validateValue(%#v) = %v", v, err)
		}
	}
	invalid := []Value{
		int(1),
		uint(1),
		[]byte("x"),
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		map[int]any{1: 1},
		make(chan int),
		func() {},
	}
	for _, v := range invalid {
		if err := validateValue(v, 0); err == nil {
			t.Fatalf("validateValue(%#v) should fail", v)
		}
	}
}

func TestValidateValueDepth(t *testing.T) {
	var v Value = int64(1)
	for i := 0; i < maxValueDepth+1; i++ {
		v = []any{v}
	}
	if err := validateValue(v, 0); err == nil {
		t.Fatal("deep value should be rejected")
	}
}

func TestStateSize(t *testing.T) {
	size, err := stateSize(nil)
	if err != nil {
		t.Fatal(err)
	}
	if size != len("null") {
		t.Fatalf("size = %d", size)
	}
	size, err = stateSize(map[string]any{"x": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if size != len(`{"x":1}`) {
		t.Fatalf("size = %d", size)
	}
}
