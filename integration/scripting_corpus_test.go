package integration_test

// Scripting corpus for the compiled-agent target.
//
// These tests are ordinary Python-C-style top-level scripts. They do not use
// the agent API and they do not claim a persistent REPL: every execution gets
// fresh guest globals, and only explicit quickpy.set_state() survives.
//
// The corpus is deliberately adaptive. A feature that is absent from the
// current artifact is recorded as a clean, catchable unsupported result
// (ImportError / AttributeError / OSError) instead of being skipped or
// reported as a pass for a feature that does not exist. When the guest adds a
// module, the same test starts exercising it without a test edit.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/keejkrej/quickpy"
)

func corpusRun(t *testing.T, r *quickpy.Runtime, source string) (quickpy.Result, error) {
	t.Helper()
	e := execute(t, r, quickpy.ExecuteRequest{Source: source, Filename: "corpus.py"})
	return wait(t, e)
}

func corpusMustRun(t *testing.T, r *quickpy.Runtime, source string) quickpy.Result {
	t.Helper()
	res, err := corpusRun(t, r, source)
	if err != nil {
		t.Fatalf("corpus script failed: %v\nsource:\n%s", err, source)
	}
	return res
}

func corpusMustRunState(t *testing.T, r *quickpy.Runtime, source string, state quickpy.Value) quickpy.Result {
	t.Helper()
	e := execute(t, r, quickpy.ExecuteRequest{Source: source, Filename: "corpus.py", InitialState: state})
	res, err := wait(t, e)
	if err != nil {
		t.Fatalf("corpus script failed: %v\nsource:\n%s", err, source)
	}
	return res
}

func corpusScriptError(t *testing.T, err error) *quickpy.ScriptErrorInfo {
	t.Helper()
	qe := requireErrorKind(t, err, quickpy.KindScriptError)
	if qe.Script == nil {
		t.Fatalf("script error has no ScriptErrorInfo: %+v", qe)
	}
	return qe.Script
}

// corpusModuleAvailable probes a module with a catchable import. It returns
// false only for a clean ImportError; any other failure is a test failure.
func corpusModuleAvailable(t *testing.T, r *quickpy.Runtime, module string) bool {
	t.Helper()
	src := fmt.Sprintf("try:\n    import %s\n    print(\"available\")\nexcept ImportError:\n    print(\"missing\")", module)
	res, err := corpusRun(t, r, src)
	if err != nil {
		t.Fatalf("probe import %s: %v", module, err)
	}
	switch out := strings.TrimSpace(textOf(res.Outputs)); out {
	case "available":
		return true
	case "missing":
		return false
	default:
		t.Fatalf("probe import %s: unexpected output %q", module, out)
		return false
	}
}

func corpusRequireImportError(t *testing.T, r *quickpy.Runtime, module string) {
	t.Helper()
	_, err := corpusRun(t, r, fmt.Sprintf("import %s", module))
	if err == nil {
		t.Fatalf("import %s succeeded, want a clean ImportError", module)
	}
	se := corpusScriptError(t, err)
	if se.Type != "ImportError" {
		t.Fatalf("import %s: script error type = %q, want ImportError (message %q)", module, se.Type, se.Message)
	}
	if !strings.Contains(se.Message, "module not found") {
		t.Fatalf("import %s: message = %q, want a clean module-not-found message", module, se.Message)
	}
}

func corpusHasAttr(t *testing.T, r *quickpy.Runtime, module, attr string) bool {
	t.Helper()
	res, err := corpusRun(t, r, fmt.Sprintf("import %s\nprint(hasattr(%s, %q))", module, module, attr))
	if err != nil {
		t.Fatalf("probe %s.%s: %v", module, attr, err)
	}
	return strings.TrimSpace(textOf(res.Outputs)) == "True"
}

func corpusLogUnsupported(t *testing.T, feature, detail string) {
	t.Helper()
	t.Logf("UNSUPPORTED (clean): %s: %s", feature, detail)
}

// corpusLines splits printed output into lines. One trailing newline is
// removed so a final empty print() remains an empty line.
func corpusLines(t *testing.T, res quickpy.Result) []string {
	t.Helper()
	out := textOf(res.Outputs)
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

func corpusRequireLines(t *testing.T, res quickpy.Result, want []string) {
	t.Helper()
	got := corpusLines(t, res)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output lines = %#v, want %#v", got, want)
	}
}

func corpusRequireFloat(t *testing.T, name, text string, want float64) {
	t.Helper()
	got, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		t.Fatalf("%s: parse %q: %v", name, text, err)
	}
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s = %v, want %v (tolerance 1e-12)", name, got, want)
	}
}

func corpusFSFacadePresent(t *testing.T, r *quickpy.Runtime) bool {
	t.Helper()
	res := corpusMustRun(t, r, `import quickpy
print(hasattr(quickpy, "fs"))`)
	return strings.TrimSpace(textOf(res.Outputs)) == "True"
}

// corpusRequireFSFacadeDenied proves that the guest quickpy.fs facade is only
// a forwarding layer: with no host fs_* tools registered, every method is a
// catchable ToolNotFoundError and no file is created.
func corpusRequireFSFacadeDenied(t *testing.T, r *quickpy.Runtime, readPath, writePath string) {
	t.Helper()
	src := fmt.Sprintf(`import asyncio, quickpy
async def main():
    calls = [
        ("read_text", (%q,)),
        ("write_text", (%q, "x")),
        ("read_bytes", (%q,)),
        ("write_bytes", (%q, b"x")),
        ("exists", (%q,)),
        ("listdir", (%q,)),
        ("mkdir", (%q,)),
        ("remove", (%q,)),
    ]
    for name, args in calls:
        try:
            await getattr(quickpy.fs, name)(*args)
            print("UNEXPECTED", name)
        except quickpy.ToolNotFoundError as e:
            print("denied", name, e.code)
quickpy.run(main())`, readPath, writePath, readPath, writePath, readPath, filepath.Dir(readPath), writePath, writePath)
	res := corpusMustRun(t, r, src)
	out := textOf(res.Outputs)
	for _, name := range []string{"read_text", "write_text", "read_bytes", "write_bytes", "exists", "listdir", "mkdir", "remove"} {
		if !strings.Contains(out, "denied "+name+" tool_not_found") {
			t.Fatalf("quickpy.fs.%s was not denied cleanly: %q", name, out)
		}
	}
	if strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("a quickpy.fs call succeeded without host tools: %q", out)
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("quickpy.fs created %s", writePath)
	}
}

// TestScriptingCorpusJSON covers json.loads/dumps with Unicode, nesting,
// None/true/false, and float64 numeric output. MicroPython dict order is hash
// order, not CPython insertion order, so the assertions are semantic rather
// than byte-exact.
func TestScriptingCorpusJSON(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if !corpusModuleAvailable(t, r, "json") {
		corpusRequireImportError(t, r, "json")
		corpusLogUnsupported(t, "json", "module not present")
		return
	}

	t.Run("dumps semantic order", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
print(json.dumps({"b": 1, "a": 2, "c": [1, 2, 3]}))`)
		out := strings.TrimSpace(textOf(res.Outputs))
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("dumps output is not valid JSON: %v (output %q)", err, out)
		}
		want := map[string]any{
			"a": float64(2),
			"b": float64(1),
			"c": []any{float64(1), float64(2), float64(3)},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dumps value = %#v, want %#v (raw %q)", got, want, out)
		}
		// Record the observed order as evidence, but do not require it.
		t.Logf("observed MicroPython dict order: %s", out)
	})

	t.Run("unicode and scalars", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
print(json.dumps({"u": "héllo ☃", "n": None, "t": True, "f": False, "x": 1.5}))`)
		out := strings.TrimSpace(textOf(res.Outputs))
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("dumps output is not valid JSON: %v (output %q)", err, out)
		}
		if got["u"] != "héllo ☃" {
			t.Fatalf("unicode value = %#v, want %q", got["u"], "héllo ☃")
		}
		if got["n"] != nil {
			t.Fatalf("None value = %#v, want nil", got["n"])
		}
		if got["t"] != true || got["f"] != false {
			t.Fatalf("bool values = %#v/%#v, want true/false", got["t"], got["f"])
		}
		if got["x"] != 1.5 {
			t.Fatalf("float value = %#v, want 1.5", got["x"])
		}
	})

	t.Run("unicode escape", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
print(json.loads('"\\u2603"'))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "☃" {
			t.Fatalf("json.loads unicode escape = %q, want %q", got, "☃")
		}
	})

	t.Run("float64 numeric output", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
print(json.dumps([1.0, 2.5, 1e20, 1e-7, 0.1]))`)
		out := strings.TrimSpace(textOf(res.Outputs))
		var got []any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("dumps output is not valid JSON: %v (output %q)", err, out)
		}
		want := []float64{1.0, 2.5, 1e20, 1e-7, 0.1}
		if len(got) != len(want) {
			t.Fatalf("float array length = %d, want %d (raw %q)", len(got), len(want), out)
		}
		for i, w := range want {
			f, ok := got[i].(float64)
			if !ok || f != w {
				t.Fatalf("float[%d] = %#v, want %v (raw %q)", i, got[i], w, out)
			}
		}
	})

	t.Run("none true false", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
print(json.dumps(None), json.dumps(True), json.dumps(False))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "null true false" {
			t.Fatalf("dumps scalars = %q, want %q", got, "null true false")
		}
		res = corpusMustRun(t, r, `import json
print(json.loads("null"), json.loads("true"), json.loads("false"))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "None True False" {
			t.Fatalf("loads scalars = %q, want %q", got, "None True False")
		}
	})

	t.Run("nested round trip", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json
v = {"nested": {"k": [None, True, False, 1.25]}, "s": "héllo ☃"}
print(json.dumps(json.loads(json.dumps(v))))`)
		out := strings.TrimSpace(textOf(res.Outputs))
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("round-trip output is not valid JSON: %v (output %q)", err, out)
		}
		want := map[string]any{
			"nested": map[string]any{"k": []any{nil, true, false, 1.25}},
			"s":      "héllo ☃",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round-trip value = %#v, want %#v (raw %q)", got, want, out)
		}
	})

	t.Run("stream dump and load", func(t *testing.T) {
		res := corpusMustRun(t, r, `import json, io
s = io.StringIO()
json.dump({"a": 1}, s)
print(s.getvalue())
pos = s.seek(0)
print(json.load(s))`)
		corpusRequireLines(t, res, []string{`{"a": 1}`, `{'a': 1}`})
	})

	t.Run("invalid json is a clean script error", func(t *testing.T) {
		_, err := corpusRun(t, r, `import json
json.loads("{bad}")`)
		se := corpusScriptError(t, err)
		if se.Type != "ValueError" || !strings.Contains(se.Message, "JSON") {
			t.Fatalf("invalid JSON error = %s: %q, want ValueError mentioning JSON", se.Type, se.Message)
		}
	})

	t.Run("cpython keyword options", func(t *testing.T) {
		// sort_keys/indent are CPython conveniences. Record whether this
		// artifact supports them; do not fail the corpus when it does not.
		for _, src := range []string{
			`import json
json.dumps({"a": 1}, sort_keys=True)`,
			`import json
json.dumps({"a": 1}, indent=2)`,
		} {
			_, err := corpusRun(t, r, src)
			if err == nil {
				t.Logf("NOTE: json keyword option is supported on this artifact")
				continue
			}
			se := corpusScriptError(t, err)
			corpusLogUnsupported(t, "json keyword option", se.Type+": "+se.Message)
		}
	})
}

// TestScriptingCorpusRegex covers groups, search, sub, and a clean pattern
// error. split/findall are exercised only when the artifact actually exposes
// them; otherwise the absence is recorded as a clean unsupported result.
func TestScriptingCorpusRegex(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if !corpusModuleAvailable(t, r, "re") {
		corpusRequireImportError(t, r, "re")
		corpusLogUnsupported(t, "re", "module not present")
		return
	}

	res := corpusMustRun(t, r, `import re
m = re.match(r"(\d+)-(\d+)", "12-34")
print(m.group(0), m.group(1), m.group(2), m.groups())`)
	if got := strings.TrimSpace(textOf(res.Outputs)); got != "12-34 12 34 ('12', '34')" {
		t.Fatalf("re groups = %q, want %q", got, "12-34 12 34 ('12', '34')")
	}

	res = corpusMustRun(t, r, `import re
print(re.search(r"b+", "abbbc").group(0))`)
	if got := strings.TrimSpace(textOf(res.Outputs)); got != "bbb" {
		t.Fatalf("re.search = %q, want %q", got, "bbb")
	}

	res = corpusMustRun(t, r, `import re
print(re.sub(r"a+", "X", "baaacaa"))`)
	if got := strings.TrimSpace(textOf(res.Outputs)); got != "bXcX" {
		t.Fatalf("re.sub = %q, want %q", got, "bXcX")
	}

	res = corpusMustRun(t, r, `import re
print(re.sub(r"(\w+)", lambda m: m.group(1).upper(), "ab cd"))`)
	if got := strings.TrimSpace(textOf(res.Outputs)); got != "AB CD" {
		t.Fatalf("re.sub callable = %q, want %q", got, "AB CD")
	}

	res = corpusMustRun(t, r, `import re
print(re.match(r"a", "b"))`)
	if got := strings.TrimSpace(textOf(res.Outputs)); got != "None" {
		t.Fatalf("re.match no-match = %q, want None", got)
	}

	_, err := corpusRun(t, r, `import re
re.match(r"(", "x")`)
	se := corpusScriptError(t, err)
	if se.Type != "ValueError" || !strings.Contains(se.Message, "regex") {
		t.Fatalf("re pattern error = %s: %q, want a clean ValueError mentioning regex", se.Type, se.Message)
	}

	if corpusHasAttr(t, r, "re", "split") {
		res = corpusMustRun(t, r, `import re
print(re.split(r"[,;]", "a,b;c"))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "['a', 'b', 'c']" {
			t.Fatalf("re.split = %q, want %q", got, "['a', 'b', 'c']")
		}
	} else {
		corpusLogUnsupported(t, "re.split", "module-level attribute absent")
	}

	// MicroPython exposes split/sub on a compiled pattern even when the
	// module-level split helper is absent.
	if corpusHasAttr(t, r, "re", "compile") {
		res = corpusMustRun(t, r, `import re
p = re.compile(r"[,;]")
print(p.split("a,b;c"))
print(p.sub("X", "a,b;c"))
q = re.compile(r"[a-z]+")
print(q.match("abc123").group(0))
print(q.search("123abc").group(0))`)
		corpusRequireLines(t, res, []string{"['a', 'b', 'c']", "aXbXc", "abc", "abc"})
	} else {
		corpusLogUnsupported(t, "re.compile", "attribute absent")
	}

	if corpusHasAttr(t, r, "re", "findall") {
		res = corpusMustRun(t, r, `import re
print(re.findall(r"\d+", "a1b22c333"))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "['1', '22', '333']" {
			t.Fatalf("re.findall = %q, want %q", got, "['1', '22', '333']")
		}
	} else {
		corpusLogUnsupported(t, "re.findall", "attribute absent")
	}
}

// TestScriptingCorpusMath covers sqrt/floor/trig accuracy and a clean domain
// error. Optional predicates are exercised only when present.
func TestScriptingCorpusMath(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if !corpusModuleAvailable(t, r, "math") {
		corpusRequireImportError(t, r, "math")
		corpusLogUnsupported(t, "math", "module not present")
		return
	}

	res := corpusMustRun(t, r, `import math
print(math.sqrt(2))
print(math.floor(-2.5))
print(math.floor(2.5))
print(math.sin(math.pi / 2))
print(math.cos(0))
print(math.tan(0))
print(math.log(math.e))
print(math.pow(2, 10))`)
	lines := corpusLines(t, res)
	if len(lines) != 8 {
		t.Fatalf("math output lines = %#v, want 8 lines", lines)
	}
	corpusRequireFloat(t, "sqrt(2)", lines[0], math.Sqrt2)
	if lines[1] != "-3" {
		t.Fatalf("floor(-2.5) = %q, want -3", lines[1])
	}
	if lines[2] != "2" {
		t.Fatalf("floor(2.5) = %q, want 2", lines[2])
	}
	corpusRequireFloat(t, "sin(pi/2)", lines[3], 1.0)
	corpusRequireFloat(t, "cos(0)", lines[4], 1.0)
	corpusRequireFloat(t, "tan(0)", lines[5], 0.0)
	corpusRequireFloat(t, "log(e)", lines[6], 1.0)
	corpusRequireFloat(t, "pow(2,10)", lines[7], 1024.0)

	_, err := corpusRun(t, r, `import math
math.sqrt(-1)`)
	se := corpusScriptError(t, err)
	if se.Type != "ValueError" || !strings.Contains(se.Message, "math domain error") {
		t.Fatalf("math domain error = %s: %q, want a clean ValueError", se.Type, se.Message)
	}

	if corpusHasAttr(t, r, "math", "isnan") && corpusHasAttr(t, r, "math", "isinf") && corpusHasAttr(t, r, "math", "isfinite") {
		res = corpusMustRun(t, r, `import math
print(math.isnan(1.0), math.isinf(1.0), math.isfinite(1.0))`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "False False True" {
			t.Fatalf("math predicates = %q, want %q", got, "False False True")
		}
	} else {
		corpusLogUnsupported(t, "math.isnan/isinf/isfinite", "one or more attributes absent")
	}
}

// TestScriptingCorpusStringIOAndText covers the in-memory text funnel and
// records csv/textwrap as clean unsupported when the frozen modules are absent.
func TestScriptingCorpusStringIOAndText(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	if !corpusModuleAvailable(t, r, "io") {
		corpusRequireImportError(t, r, "io")
		corpusLogUnsupported(t, "io.StringIO", "io module not present")
	} else if !corpusHasAttr(t, r, "io", "StringIO") {
		corpusLogUnsupported(t, "io.StringIO", "attribute absent")
	} else {
		res := corpusMustRun(t, r, `import io
s = io.StringIO()
print("A", s.write("hello"))
print("B", s.write(" world"))
print("C", s.getvalue())
print("D", s.seek(0))
print("E", s.read(5))
print("F", s.read())
print("G", s.seek(0))
print("H", s.readline())
print("I", s.readline())`)
		corpusRequireLines(t, res, []string{
			"A 5",
			"B 6",
			"C hello world",
			"D 0",
			"E hello",
			"F  world",
			"G 0",
			"H hello world",
			"I ",
		})

		// readlines is not part of the minimum StringIO surface. Exercise it
		// only when the artifact exposes it.
		res, err := corpusRun(t, r, `import io
print(io.StringIO("a\nb\n").readlines())`)
		if err != nil {
			se := corpusScriptError(t, err)
			if se.Type != "AttributeError" {
				t.Fatalf("StringIO.readlines: unexpected error %s: %q", se.Type, se.Message)
			}
			corpusLogUnsupported(t, "io.StringIO.readlines", "attribute absent")
		} else if got := strings.TrimSpace(textOf(res.Outputs)); got != "['a\\n', 'b\\n']" {
			t.Fatalf("StringIO.readlines = %q, want %q", got, "['a\\n', 'b\\n']")
		}
	}

	if !corpusModuleAvailable(t, r, "csv") {
		corpusRequireImportError(t, r, "csv")
		corpusLogUnsupported(t, "csv", "module not present")
	} else {
		res := corpusMustRun(t, r, `import csv, io
rows = list(csv.reader(io.StringIO("a,b\n1,2\n")))
print(rows)
out = io.StringIO()
w = csv.writer(out)
w.writerow(["x", "y,z"])
print(repr(out.getvalue()))`)
		out := textOf(res.Outputs)
		for _, want := range []string{"a", "b", "1", "2", "x", "y,z"} {
			if !strings.Contains(out, want) {
				t.Fatalf("csv output %q does not contain %q", out, want)
			}
		}
	}

	if !corpusModuleAvailable(t, r, "textwrap") {
		corpusRequireImportError(t, r, "textwrap")
		corpusLogUnsupported(t, "textwrap", "module not present")
	} else {
		if corpusHasAttr(t, r, "textwrap", "dedent") {
			res := corpusMustRun(t, r, `import textwrap
print(repr(textwrap.dedent("    a\n      b\n    c\n")))`)
			out := strings.TrimSpace(textOf(res.Outputs))
			for _, want := range []string{"a", "b", "c"} {
				if !strings.Contains(out, want) {
					t.Fatalf("textwrap.dedent output %q does not contain %q", out, want)
				}
			}
		} else {
			corpusLogUnsupported(t, "textwrap.dedent", "attribute absent")
		}
		if corpusHasAttr(t, r, "textwrap", "wrap") {
			res := corpusMustRun(t, r, `import textwrap
print(textwrap.wrap("the quick brown fox", 10))`)
			out := strings.TrimSpace(textOf(res.Outputs))
			for _, want := range []string{"the quick", "brown fox"} {
				if !strings.Contains(out, want) {
					t.Fatalf("textwrap.wrap output %q does not contain %q", out, want)
				}
			}
		} else {
			corpusLogUnsupported(t, "textwrap.wrap", "attribute absent")
		}
	}
}

// TestScriptingCorpusBinary covers hex/base64 conversion with bytes and text
// inputs. The round trip must preserve every byte, including NUL and 0xff.
func TestScriptingCorpusBinary(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	if !corpusModuleAvailable(t, r, "binascii") {
		corpusRequireImportError(t, r, "binascii")
		corpusLogUnsupported(t, "binascii", "module not present")
	} else {
		res := corpusMustRun(t, r, `import binascii
data = b"\x00\x01\xfe\xffhello"
h = binascii.hexlify(data)
print(h)
print(binascii.unhexlify(h) == data)
b = binascii.b2a_base64(data)
print(b)
print(binascii.a2b_base64(b) == data)
print(binascii.hexlify(b"AB"))
print(binascii.unhexlify(b"4142"))
print(binascii.b2a_base64(b"hello"))
print(binascii.a2b_base64(b"aGVsbG8="))
print(binascii.hexlify("hello"))
print(binascii.unhexlify("68656c6c6f"))
print(binascii.b2a_base64("hello"))
print(binascii.a2b_base64("aGVsbG8="))`)
		corpusRequireLines(t, res, []string{
			"b'0001feff68656c6c6f'",
			"True",
			"b'AAH+/2hlbGxv\\n'",
			"True",
			"b'4142'",
			"b'AB'",
			"b'aGVsbG8=\\n'",
			"b'hello'",
			"b'68656c6c6f'",
			"b'hello'",
			"b'aGVsbG8=\\n'",
			"b'hello'",
		})

		_, err := corpusRun(t, r, `import binascii
binascii.unhexlify(b"zz")`)
		se := corpusScriptError(t, err)
		if se.Type != "ValueError" || !strings.Contains(se.Message, "non-hex") {
			t.Fatalf("binascii invalid hex = %s: %q, want a clean ValueError", se.Type, se.Message)
		}
	}

	if !corpusModuleAvailable(t, r, "base64") {
		corpusRequireImportError(t, r, "base64")
		corpusLogUnsupported(t, "base64", "module not present")
	} else {
		res := corpusMustRun(t, r, `import base64
print(base64.b64encode(b"hello"))
print(base64.b64decode(b"aGVsbG8="))`)
		out := textOf(res.Outputs)
		if !strings.Contains(out, "aGVsbG8=") || !strings.Contains(out, "b'hello'") {
			t.Fatalf("base64 output = %q, want encoded and decoded hello", out)
		}
	}

	res := corpusMustRun(t, r, `print(hasattr(b"", "hex"))
print(hasattr(bytearray(), "hex"))`)
	lines := corpusLines(t, res)
	if len(lines) != 2 {
		t.Fatalf("hex attribute probe = %#v, want 2 lines", lines)
	}
	if lines[0] == "True" {
		res = corpusMustRun(t, r, `print(b"AB".hex())`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "4142" {
			t.Fatalf("bytes.hex = %q, want 4142", got)
		}
	} else {
		corpusLogUnsupported(t, "bytes.hex", "attribute absent")
	}
	if lines[1] == "True" {
		res = corpusMustRun(t, r, `print(bytearray(b"AB").hex())`)
		if got := strings.TrimSpace(textOf(res.Outputs)); got != "4142" {
			t.Fatalf("bytearray.hex = %q, want 4142", got)
		}
	} else {
		corpusLogUnsupported(t, "bytearray.hex", "attribute absent")
	}
}

// TestScriptingCorpusDefaultDeny proves the default-deny posture: process,
// path, OS, and network modules are absent; open() is a clean OSError; the
// quickpy.fs facade is present but inert (no host fs_* tools are registered,
// so every method is a catchable ToolNotFoundError); and unregistered tool
// calls are catchable ToolNotFoundError values that create no files, change no
// environment, and leak no host data.
func TestScriptingCorpusDefaultDeny(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	for _, module := range []string{
		"subprocess", "pathlib", "os", "uos", "gc", "socket", "shutil",
		"tempfile", "multiprocessing", "threading",
	} {
		corpusRequireImportError(t, r, module)
	}

	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("SECRET_CORPUS"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePath := filepath.Join(dir, "write.txt")

	src := fmt.Sprintf(`try:
    f = open(%q, "r")
    print("READ", f.read())
except OSError as e:
    print("OSError", str(e))`, secretPath)
	res := corpusMustRun(t, r, src)
	out := textOf(res.Outputs)
	if !strings.Contains(out, "OSError") || !strings.Contains(out, "open is not supported") {
		t.Fatalf("open read output = %q, want a clean OSError", out)
	}
	if strings.Contains(out, "SECRET_CORPUS") {
		t.Fatalf("guest read a host file: %q", out)
	}

	src = fmt.Sprintf(`try:
    open(%q, "w").write("x")
    print("WROTE")
except OSError as e:
    print("OSError", str(e))`, writePath)
	res = corpusMustRun(t, r, src)
	if !strings.Contains(textOf(res.Outputs), "open is not supported") {
		t.Fatalf("open write output = %q, want a clean OSError", textOf(res.Outputs))
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("guest created %s", writePath)
	}

	_, err := corpusRun(t, r, fmt.Sprintf(`open(%q, "w")`, writePath))
	se := corpusScriptError(t, err)
	if se.Type != "OSError" || !strings.Contains(se.Message, "open is not supported") {
		t.Fatalf("uncaught open = %s: %q, want a clean OSError", se.Type, se.Message)
	}

	if corpusFSFacadePresent(t, r) {
		// The guest facade exists, but the host fs_* tools are not
		// registered. Every call must be a catchable ToolNotFoundError.
		corpusRequireFSFacadeDenied(t, r, secretPath, writePath)
	} else {
		corpusLogUnsupported(t, "quickpy.fs", "facade absent")
	}

	envName := "QUICKPY_CORPUS_SENTINEL"
	os.Unsetenv(envName)
	t.Cleanup(func() { os.Unsetenv(envName) })

	src = fmt.Sprintf(`import asyncio, quickpy
async def main():
    attempts = [
        ("fs_read", {"path": %q}),
        ("fs_write", {"path": %q, "data": "x"}),
        ("http_get", {"url": "http://127.0.0.1:1/"}),
        ("env_get", {"name": %q}),
        ("symlink", {"target": %q, "link": %q}),
    ]
    for name, kwargs in attempts:
        try:
            await getattr(quickpy.tools, name)(**kwargs)
            print("UNEXPECTED", name)
        except quickpy.ToolNotFoundError as e:
            print("denied", name, e.code)
quickpy.run(main())`, secretPath, writePath, envName, secretPath, writePath+".link")
	res = corpusMustRun(t, r, src)
	out = textOf(res.Outputs)
	for _, name := range []string{"fs_read", "fs_write", "http_get", "env_get", "symlink"} {
		if !strings.Contains(out, "denied "+name+" tool_not_found") {
			t.Fatalf("tool attempt %s was not denied cleanly: %q", name, out)
		}
	}
	if strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("an unregistered tool call succeeded: %q", out)
	}
	if strings.Contains(out, "SECRET_CORPUS") {
		t.Fatalf("tool attempt leaked host file data: %q", out)
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("tool attempt created %s", writePath)
	}
	if _, err := os.Stat(writePath + ".link"); !os.IsNotExist(err) {
		t.Fatalf("tool attempt created %s.link", writePath)
	}
	if got := os.Getenv(envName); got != "" {
		t.Fatalf("tool attempt changed host environment %s=%q", envName, got)
	}
	if res.ToolCalls != 5 {
		t.Fatalf("ToolCalls = %d, want 5 denied attempts", res.ToolCalls)
	}

	res = corpusMustRun(t, r, `import asyncio
try:
    asyncio.open_connection("127.0.0.1", 80)
except asyncio.UnsupportedError as e:
    print("stream", str(e))`)
	if !strings.Contains(textOf(res.Outputs), "stream I/O is not supported") {
		t.Fatalf("asyncio stream I/O was not blocked: %q", textOf(res.Outputs))
	}
}

// TestScriptingCorpusIdentityAndFreshness checks honest guest identity, zero
// imports, fresh globals per call, and explicit-only state. It does not claim
// a persistent REPL.
func TestScriptingCorpusIdentityAndFreshness(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	res := corpusMustRun(t, r, `import sys
print(sys.version)
print(sys.implementation)
print(sys.byteorder)`)
	out := textOf(res.Outputs)
	if !strings.Contains(out, "MicroPython") {
		t.Fatalf("sys.version does not identify MicroPython: %q", out)
	}
	if strings.Contains(out, "CPython") {
		t.Fatalf("sys.version falsely identifies CPython: %q", out)
	}
	if !strings.Contains(out, "micropython") {
		t.Fatalf("sys.implementation does not identify micropython: %q", out)
	}
	if !strings.Contains(out, "wasm32") {
		t.Fatalf("sys.implementation does not report the wasm32 guest architecture: %q", out)
	}
	if !strings.Contains(out, "little") {
		t.Fatalf("sys.byteorder is not reported: %q", out)
	}
	if len(res.Guest.Imports) != 0 {
		t.Fatalf("guest imports = %+v, want none", res.Guest.Imports)
	}

	res, err := corpusRun(t, r, `import sys
print(sys.platform)`)
	if err != nil {
		se := corpusScriptError(t, err)
		if se.Type != "AttributeError" {
			t.Fatalf("sys.platform: unexpected error %s: %q", se.Type, se.Message)
		}
		corpusLogUnsupported(t, "sys.platform", "attribute absent; sys.implementation reports wasm32-wasi")
	} else {
		platform := strings.TrimSpace(textOf(res.Outputs))
		if platform == "" {
			t.Fatalf("sys.platform is empty")
		}
		for _, bad := range []string{"linux", "linux2", "darwin", "win32", "cygwin"} {
			if platform == bad {
				t.Fatalf("sys.platform = %q impersonates a host OS", platform)
			}
		}
	}

	// Globals are fresh per call, exactly like a python-c namespace.
	corpusMustRun(t, r, `x = 1
print("set")`)
	_, err = corpusRun(t, r, `print(x)`)
	se := corpusScriptError(t, err)
	if se.Type != "NameError" {
		t.Fatalf("global leaked across calls: error = %s: %q, want NameError", se.Type, se.Message)
	}

	// Imports are also per-call.
	corpusMustRun(t, r, `import json
print("imported")`)
	_, err = corpusRun(t, r, `print(json.dumps({}))`)
	se = corpusScriptError(t, err)
	if se.Type != "NameError" {
		t.Fatalf("import leaked across calls: error = %s: %q, want NameError", se.Type, se.Message)
	}

	// State is explicit. The runtime is stateless by default: a committed
	// state is returned to the caller and must be passed back as
	// InitialState. It is not an implicit persistent REPL.
	res = corpusMustRun(t, r, `import quickpy
quickpy.set_state({"k": 1})
print("state")`)
	requireState(t, res, map[string]any{"k": int64(1)})
	res = corpusMustRunState(t, r, `import quickpy
print(quickpy.get_state())`, res.State)
	if got := strings.TrimSpace(textOf(res.Outputs)); !strings.Contains(got, "'k': 1") {
		t.Fatalf("get_state output = %q, want a dict containing 'k': 1", got)
	}

	for _, module := range []string{"os", "subprocess", "socket", "pathlib"} {
		corpusRequireImportError(t, r, module)
	}
}

// TestScriptingCorpusFilesystemDeferred records the ambient filesystem
// posture: open() read/write are cleanly unsupported, the quickpy.fs facade is
// present but inert without host fs_* registration, and the practical future
// wrapper names are denied rather than silently succeeding. This is evidence
// for the opt-in wrapper layer, not a claim that ambient filesystem support
// exists or that M2 filesystem work is complete.
func TestScriptingCorpusFilesystemDeferred(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	dir := t.TempDir()
	readPath := filepath.Join(dir, "read.txt")
	writePath := filepath.Join(dir, "write.txt")
	if err := os.WriteFile(readPath, []byte("SECRET_CORPUS"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := fmt.Sprintf(`try:
    f = open(%q, "r")
    print("READ", f.read())
except OSError as e:
    print("OSError", str(e))`, readPath)
	res := corpusMustRun(t, r, src)
	out := textOf(res.Outputs)
	if !strings.Contains(out, "open is not supported") {
		t.Fatalf("filesystem read output = %q, want a clean OSError", out)
	}
	if strings.Contains(out, "SECRET_CORPUS") {
		t.Fatalf("filesystem read leaked host data: %q", out)
	}

	src = fmt.Sprintf(`try:
    open(%q, "w").write("x")
    print("WROTE")
except OSError as e:
    print("OSError", str(e))`, writePath)
	res = corpusMustRun(t, r, src)
	if !strings.Contains(textOf(res.Outputs), "open is not supported") {
		t.Fatalf("filesystem write output = %q, want a clean OSError", textOf(res.Outputs))
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("filesystem write created %s", writePath)
	}

	for _, module := range []string{"os", "pathlib", "shutil", "tempfile", "glob"} {
		corpusRequireImportError(t, r, module)
	}

	if corpusFSFacadePresent(t, r) {
		// The facade is present, but the host fs_* tools are not registered.
		// This is still default-deny, not a filesystem profile.
		corpusRequireFSFacadeDenied(t, r, readPath, writePath)
	} else {
		corpusLogUnsupported(t, "quickpy.fs", "facade absent")
	}

	src = fmt.Sprintf(`import asyncio, quickpy
async def main():
    for name, kwargs in [
        ("fs_read", {"path": %q}),
        ("fs_write", {"path": %q, "data": "x"}),
        ("fs_list", {"path": %q}),
        ("fs_stat", {"path": %q}),
    ]:
        try:
            await getattr(quickpy.tools, name)(**kwargs)
            print("UNEXPECTED", name)
        except quickpy.ToolNotFoundError as e:
            print("deferred", name, e.code)
quickpy.run(main())`, readPath, writePath, dir, readPath)
	res = corpusMustRun(t, r, src)
	out = textOf(res.Outputs)
	for _, name := range []string{"fs_read", "fs_write", "fs_list", "fs_stat"} {
		if !strings.Contains(out, "deferred "+name+" tool_not_found") {
			t.Fatalf("future wrapper %s was not denied cleanly: %q", name, out)
		}
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("future wrapper created %s", writePath)
	}

	res = corpusMustRun(t, r, `import asyncio
try:
    asyncio.open_connection("127.0.0.1", 80)
except asyncio.UnsupportedError as e:
    print("stream", str(e))`)
	if !strings.Contains(textOf(res.Outputs), "stream I/O is not supported") {
		t.Fatalf("asyncio stream I/O was not blocked: %q", textOf(res.Outputs))
	}
}

// TestScriptingCorpusModuleMatrix logs the observed support matrix. It is
// evidence for the corpus document and for the next wrapper decision; it does
// not fail when a feature is absent.
func TestScriptingCorpusModuleMatrix(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})

	modules := []string{
		"json", "re", "math", "binascii", "io", "csv", "textwrap", "base64",
		"os", "uos", "gc", "subprocess", "pathlib", "socket", "shutil",
		"tempfile", "glob", "threading", "multiprocessing",
	}
	for _, module := range modules {
		status := "UNSUPPORTED"
		if corpusModuleAvailable(t, r, module) {
			status = "SUPPORTED"
		}
		t.Logf("MATRIX module %-16s %s", module, status)
	}

	attrs := []struct{ module, attr string }{
		{"re", "match"}, {"re", "search"}, {"re", "sub"}, {"re", "split"},
		{"re", "findall"}, {"re", "IGNORECASE"}, {"re", "DOTALL"},
		{"math", "sqrt"}, {"math", "floor"}, {"math", "sin"},
		{"math", "isnan"}, {"math", "isinf"}, {"math", "isfinite"},
		{"io", "StringIO"}, {"binascii", "hexlify"}, {"binascii", "b2a_base64"},
	}
	for _, probe := range attrs {
		status := "UNSUPPORTED"
		if corpusHasAttr(t, r, probe.module, probe.attr) {
			status = "SUPPORTED"
		}
		t.Logf("MATRIX %s.%-12s %s", probe.module, probe.attr, status)
	}
}
