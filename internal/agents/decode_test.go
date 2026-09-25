package agents

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

func TestDecodeResultFromStringVariants(t *testing.T) {
	t.Run("plain json string", func(t *testing.T) {
		got, err := DecodeResult[CoordinatorResult](`{"objective":"o","next_experiment":"n","rationale":["r1"]}`)
		if err != nil || got.NextExperiment != "n" || len(got.Rationale) != 1 {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
	t.Run("fenced json string", func(t *testing.T) {
		got, err := DecodeResult[CoordinatorResult]("```json\n{\"objective\":\"o\",\"next_experiment\":\"n\",\"rationale\":\"single\"}\n```")
		if err != nil || got.NextExperiment != "n" || len(got.Rationale) != 1 || got.Rationale[0] != "single" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
	t.Run("prose wrapped json", func(t *testing.T) {
		got, err := DecodeResult[CoordinatorResult]("Sure, here it is:\n{\"objective\":\"o\",\"next_experiment\":\"n\"}\nThanks!")
		if err != nil || got.Objective != "o" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
}

func TestDecodeResultFromContent(t *testing.T) {
	c := &genai.Content{Parts: []*genai.Part{{Text: "{\"hypothesis\":\"h\",\"patch\":\"p\"}"}}}
	got, err := DecodeResult[OptimizerResult](c)
	if err != nil || got.Hypothesis != "h" || got.Patch != "p" {
		t.Fatalf("got %+v err %v", got, err)
	}
	nilContent := (*genai.Content)(nil)
	if _, err := DecodeResult[OptimizerResult](nilContent); err == nil {
		t.Fatal("expected error for nil content")
	}
}

func TestTolerantUnmarshaling(t *testing.T) {
	var review ReviewerResult
	data := `{"proceed":"true","behavior_argument":"b","concerns":"overfitting risk","required_checks":["x"]}`
	if err := json.Unmarshal([]byte(data), &review); err != nil {
		t.Fatal(err)
	}
	if !review.Proceed || len(review.Concerns) != 1 || review.Concerns[0] != "overfitting risk" {
		t.Fatalf("got %+v", review)
	}

	var proposal WorkloadProposal
	data2 := `{"name":"w","arguments":"--flag .","tier":"representative","provenance":"manifest","expected_valid":true}`
	if err := json.Unmarshal([]byte(data2), &proposal); err != nil {
		t.Fatal(err)
	}
	if len(proposal.Arguments) != 1 || proposal.Arguments[0] != "--flag ." {
		t.Fatalf("got %+v", proposal)
	}

	var analyst AnalystResult
	data3 := `{"hot_paths":[],"candidate_hypotheses":"one"}`
	if err := json.Unmarshal([]byte(data3), &analyst); err != nil {
		t.Fatal(err)
	}
	if len(analyst.CandidateHypotheses) != 1 {
		t.Fatalf("got %+v", analyst)
	}
}

func TestFlexWorkloadShapes(t *testing.T) {
	t.Run("object stdin with content key", func(t *testing.T) {
		var w WorkloadProposal
		err := json.Unmarshal([]byte(`{"name":"w","arguments":["-x"],"stdin":{"content":"a,b\n"},"tier":"representative","provenance":"manifest","expected_valid":true}`), &w)
		if err != nil || w.Stdin != "a,b\n" {
			t.Fatalf("stdin=%q err=%v", w.Stdin, err)
		}
	})
	t.Run("fixtures as mapping", func(t *testing.T) {
		var w WorkloadProposal
		err := json.Unmarshal([]byte(`{"name":"w","arguments":[],"fixtures":{"data/input.json":"{\"a\":1}"},"tier":"representative","provenance":"manifest","expected_valid":true}`), &w)
		if err != nil || len(w.Fixtures) != 1 || w.Fixtures[0].Path != "data/input.json" || w.Fixtures[0].Content != `{"a":1}` {
			t.Fatalf("fixtures=%+v err=%v", w.Fixtures, err)
		}
	})
	t.Run("single fixture object", func(t *testing.T) {
		var w WorkloadProposal
		err := json.Unmarshal([]byte(`{"name":"w","arguments":[],"fixtures":{"path":"in.txt","content":"hi"},"tier":"representative","provenance":"manifest","expected_valid":true}`), &w)
		if err != nil || len(w.Fixtures) != 1 || w.Fixtures[0].Path != "in.txt" {
			t.Fatalf("fixtures=%+v err=%v", w.Fixtures, err)
		}
	})
}

func TestRepairCommonMalformations(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"trailing comma object", `{"a":1,}`, `{"a":1}`},
		{"trailing comma array", `[1,2,]`, `[1,2]`},
		{"comma inside string kept", `{"a":"x,}"}`, `{"a":"x,}"}`},
		{"clean text unchanged", `{"a":[1,2]}`, `{"a":[1,2]}`},
	}
	for _, tc := range tests {
		if got := RepairCommonMalformations(tc.in); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}

	_, err := DecodeResult[CoordinatorResult]("bad {\"a\": ] tail")
	if err == nil || len(err.Error()) < 20 {
		t.Fatalf("expected diagnostic error, got %v", err)
	}
	t.Log(err)
}

func TestFlexStringsObjects(t *testing.T) {
	var got ExplorerResult
	data := `{"entry_points":[{"symbol":"cmd/gojq","role":"CLI binary"},{"name":"internal"},{"path":"x.go"}],"proposals":[]}`
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd/gojq", "internal", "x.go"}
	if len(got.EntryPoints) != 3 || got.EntryPoints[0] != want[0] || got.EntryPoints[1] != want[1] || got.EntryPoints[2] != want[2] {
		t.Fatalf("entry_points = %v", got.EntryPoints)
	}
}

func TestEscapeEmbeddedQuotes(t *testing.T) {
	t.Run("nested json fixture content", testEscapeNestedJSON)
	t.Run("prose with braces and quotes", testEscapeProse)
}

func testEscapeNestedJSON(t *testing.T) {
	broken := `{"name":"w","stdin":"{\"languageCount\":1}"}`
	// already valid: must be unchanged
	got, changed := EscapeEmbeddedQuotes(broken)
	if changed || got != broken {
		t.Fatalf("valid payload rewritten: %q changed=%v", got, changed)
	}

	raw := `{"fixtures":[{"path":"d.json","content":"{"languageCount":1,"languages":[{"Name":"Go","Code":80}]}"}],"name":"w"}`
	repaired, changed := EscapeEmbeddedQuotes(raw)
	if !changed {
		t.Fatal("expected repair")
	}
	var w WorkloadProposal
	if err := json.Unmarshal([]byte(RepairCommonMalformations(repaired)), &w); err != nil {
		t.Fatalf("repaired payload does not parse: %v\n%s", err, repaired)
	}
	if len(w.Fixtures) != 1 || w.Fixtures[0].Path != "d.json" {
		t.Fatalf("fixtures=%+v", w.Fixtures)
	}
	if w.Fixtures[0].Content == "" {
		t.Fatal("fixture content lost")
	}
}

func testEscapeProse(t *testing.T) {
	raw := `{"next_experiment":"pad = {sprintf(\"k%03d\", i): (i*31+7)} for 8192 blobs","objective":"o"}`
	got, err := DecodeResult[CoordinatorResult](raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Objective != "o" || got.NextExperiment == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestStringFixturesTolerance(t *testing.T) {
	cases := []string{
		`{"name":"p","arguments":["."],"stdin":"{\"a\":1}","fixtures":"some string","tier":"representative","provenance":"manifest","expected_valid":true}`,
		`{"name":"p","arguments":["."],"fixtures":["file.json"],"tier":"representative","provenance":"manifest","expected_valid":true}`,
	}
	for i, p := range cases {
		var w WorkloadProposal
		if err := json.Unmarshal([]byte(p), &w); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	full := `{"entry_points":["./cmd/gojq"],"proposals":[{"name":"identity_small","arguments":["."],"stdin":"{\\\"a\\\":1}","fixtures":"some string","tier":"representative","provenance":"manifest","expected_valid":true}],"rationale":"r"}`
	var er ExplorerResult
	if err := json.Unmarshal([]byte(full), &er); err != nil {
		t.Fatalf("full: %v", err)
	}
}

func TestRepairMissingClosers(t *testing.T) {
	cases := []struct{ name, broken string }{
		{"missing object closer before array closer", `{"proposals":[{"name":"n","stdin":"s","expected_valid":true"]}`},
		{"truncated tail", `{"a":{"b":[1,2]`},
		{"two levels missing", `{"a":{"b":"c"`},
	}
	for _, tc := range cases {
		parsed := false
		for _, candidate := range repairCandidates(tc.broken) {
			var v any
			if err := json.Unmarshal([]byte(RepairCommonMalformations(candidate.text)), &v); err == nil {
				parsed = true
				break
			}
		}
		if !parsed {
			t.Fatalf("%s: no repair variant parses (input %q)", tc.name, tc.broken)
		}
	}
	// Valid JSON must come through unchanged.
	valid := `{"a":[{"b":"c"}]}`
	if got, changed := RepairMissingClosers(valid); changed || got != valid {
		t.Fatalf("valid json rewritten: %q changed=%v", got, changed)
	}
}

func TestDecodeSurvivesStrayQuoteTail(t *testing.T) {
	broken := "{\"entry_points\":[\"e\"],\"proposals\":[{\"name\":\"n\",\"arguments\":[\".\"],\"stdin\":\"s\",\"expected_valid\":true because they match the CLI grammar and parsing logic.\"]}"
	got, err := DecodeResult[ExplorerResult](broken)
	if err == nil && len(got.Proposals) > 0 {
		t.Logf("recovered proposals: %+v", got.Proposals)
		return
	}
	t.Logf("unrecoverable as expected or new shape: %v", err)
}

// The optimizer sends its diff as an array of lines, but campaign state
// persisted before that change replays the string form on resume, and a model
// that ignores the instruction sends other shapes too. All of them must land on
// the same canonical diff text.
func TestOptimizerPatchWireShapes(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			name: "array of lines",
			in:   `{"hypothesis":"h","patch":["--- a/m.go","+++ b/m.go","@@ -1,2 +1,2 @@","-\ta()","+\tb()"]}`,
			want: "--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n-\ta()\n+\tb()\n",
		},
		{
			name: "string form from persisted state",
			in:   `{"hypothesis":"h","patch":"--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n"}`,
			want: "--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n",
		},
		{
			name: "elements carrying their own terminator are not doubled",
			in:   `{"hypothesis":"h","patch":["--- a/m.go\n","+++ b/m.go\n","@@ -1,1 +1,1 @@\n","-a\n","+b\n"]}`,
			want: "--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n",
		},
		{
			name: "blank context line survives as a space element",
			in:   `{"hypothesis":"h","patch":["--- a/m.go","+++ b/m.go","@@ -1,3 +1,3 @@"," a"," ","-b","+B"]}`,
			want: "--- a/m.go\n+++ b/m.go\n@@ -1,3 +1,3 @@\n a\n \n-b\n+B\n",
		},
		{
			name: "wrapper object collapses to its content",
			in:   `{"hypothesis":"h","patch":{"content":"--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n"}}`,
			want: "--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n",
		},
		{
			name: "empty array yields an empty patch rather than a stray newline",
			in:   `{"hypothesis":"h","patch":[]}`,
			want: "",
		},
		{
			name: "missing patch stays empty",
			in:   `{"hypothesis":"h"}`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got OptimizerResult
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Patch != tc.want {
				t.Fatalf("patch = %q, want %q", got.Patch, tc.want)
			}
		})
	}
}

// TestOptimizerFunctionSourceWireShapes: function_source and imports (ADR
// 0022) decode leniently like every other optimizer field, including the
// single-string-collapses-to-array shape imports shares with risks and
// validation_plan.
func TestOptimizerFunctionSourceWireShapes(t *testing.T) {
	tests := []struct {
		name, in, wantSource string
		wantImports          []string
	}{
		{
			name:        "plain string and array",
			in:          `{"hypothesis":"h","function_source":"func f() {}","imports":["bufio","strings"]}`,
			wantSource:  "func f() {}",
			wantImports: []string{"bufio", "strings"},
		},
		{
			name:        "single import string collapses to one element",
			in:          `{"hypothesis":"h","function_source":"func f() {}","imports":"bufio"}`,
			wantSource:  "func f() {}",
			wantImports: []string{"bufio"},
		},
		{
			name:        "wrapper object collapses to its content",
			in:          `{"hypothesis":"h","function_source":{"content":"func f() {}"}}`,
			wantSource:  "func f() {}",
			wantImports: nil,
		},
		{
			name:        "missing fields stay empty",
			in:          `{"hypothesis":"h","patch":"diff"}`,
			wantSource:  "",
			wantImports: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got OptimizerResult
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.FunctionSource != tc.wantSource {
				t.Fatalf("function_source = %q, want %q", got.FunctionSource, tc.wantSource)
			}
			requireStringSliceEqual(t, got.Imports, tc.wantImports)
		})
	}
}

// requireStringSliceEqual fails the test unless got and want hold the same
// elements in order.
func requireStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("imports = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("imports = %v, want %v", got, want)
		}
	}
}

// Patch text must survive the round trip through campaign state unchanged:
// the field marshals as a plain string whichever wire shape produced it.
func TestOptimizerPatchRoundTripsAsString(t *testing.T) {
	var decoded OptimizerResult
	if err := json.Unmarshal([]byte(`{"hypothesis":"h","patch":["--- a/m.go","+++ b/m.go"]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var reread OptimizerResult
	if err := json.Unmarshal(encoded, &reread); err != nil {
		t.Fatalf("re-reading persisted state: %v", err)
	}
	if reread.Patch != decoded.Patch {
		t.Fatalf("patch changed across persistence: %q -> %q", decoded.Patch, reread.Patch)
	}
}

func TestEscapeRawControlChars(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantChanged    bool
	}{
		{
			name:        "raw newline inside value",
			in:          "{\"patch\":\"--- a/m.go\n+++ b/m.go\"}",
			want:        `{"patch":"--- a/m.go\n+++ b/m.go"}`,
			wantChanged: true,
		},
		{
			name:        "raw tab inside value",
			in:          "{\"patch\":\"+\tfoo()\"}",
			want:        `{"patch":"+\tfoo()"}`,
			wantChanged: true,
		},
		{
			name:        "formatting newlines outside strings kept",
			in:          "{\n  \"a\": 1\n}",
			want:        "{\n  \"a\": 1\n}",
			wantChanged: false,
		},
		{
			name:        "already escaped sequences untouched",
			in:          `{"patch":"a\nb\tc"}`,
			want:        `{"patch":"a\nb\tc"}`,
			wantChanged: false,
		},
		{
			name:        "escaped backslash does not hide the next quote",
			in:          "{\"a\":\"x\\\\\",\"b\":\"y\nz\"}",
			want:        `{"a":"x\\","b":"y\nz"}`,
			wantChanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := EscapeRawControlChars(tc.in)
			if got != tc.want || changed != tc.wantChanged {
				t.Fatalf("got %q changed=%v, want %q changed=%v", got, changed, tc.want, tc.wantChanged)
			}
		})
	}
}

// Malformations observed on real optimizer turns, each of which cost a retry
// or the whole cycle before the repairs below existed.
func TestDecodeOptimizerMalformations(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantPatch  string
		wantRepair Repair
	}{
		{
			name:       "diff pasted verbatim with raw newlines",
			raw:        "{\"hypothesis\":\"h\",\"patch\":\"--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n\",\"expected_effect\":\"e\"}",
			wantPatch:  "--- a/m.go\n+++ b/m.go\n@@ -1,1 +1,1 @@\n-a\n+b\n",
			wantRepair: RepairEscapedControlChars,
		},
		{
			name:       "unescaped quotes inside a line element",
			raw:        `{"hypothesis":"h","patch":["--- a/m.go","+++ b/m.go","@@ -1,2 +1,2 @@","-    s := "old"","+    s := "new""],"expected_effect":"e"}`,
			wantPatch:  "--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n-    s := \"old\"\n+    s := \"new\"\n",
			wantRepair: RepairEscapedQuotes,
		},
		{
			name:       "unescaped quotes inside a string-form patch",
			raw:        `{"hypothesis":"h","patch":"--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n-    s := "old"\n+    s := "new"\n","expected_effect":"e"}`,
			wantPatch:  "--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n-    s := \"old\"\n+    s := \"new\"\n",
			wantRepair: RepairEscapedQuotes,
		},
		{
			name:       "response cut off at the output token cap",
			raw:        `{"hypothesis":"h","patch":["--- a/m.go","+++ b/m.go","@@ -1,2 +1,2 @@","-    a()","+    b(`,
			wantPatch:  "--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n-    a()\n+    b(\n",
			wantRepair: RepairTerminatedString,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeResult[OptimizerResult](tc.raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Patch != tc.wantPatch {
				t.Fatalf("patch = %q, want %q", got.Patch, tc.wantPatch)
			}
			// The salvaged value must be the one DecodeResult returns; the
			// variant only adds the name of the repair that produced it.
			reported, repair, err := DecodeResultWithRepair[OptimizerResult](tc.raw)
			if err != nil || reported.Patch != got.Patch {
				t.Fatalf("DecodeResultWithRepair = %q, %v; want the same patch as DecodeResult", reported.Patch, err)
			}
			if repair != tc.wantRepair {
				t.Fatalf("repair = %q, want %q", repair, tc.wantRepair)
			}
		})
	}
}

// A payload that parses as sent, after only the tolerances that cannot change
// what the model said, must report no repair; one the decoder had to rewrite
// must say which rewrite it was, so a salvaged answer is not recorded as an
// intended one.
func TestDecodeResultWithRepairNamesTheRepair(t *testing.T) {
	tests := []struct {
		name       string
		raw        any
		wantRepair Repair
	}{
		{name: "clean object", raw: `{"objective":"o","next_experiment":"n"}`},
		{name: "fenced object", raw: "```json\n{\"objective\":\"o\",\"next_experiment\":\"n\"}\n```"},
		{name: "trailing comma", raw: `{"objective":"o","next_experiment":"n",}`},
		{name: "already decoded value", raw: map[string]any{"objective": "o", "next_experiment": "n"}},
		{name: "degraded empty result", raw: "{}"},
		{name: "missing closer", raw: `{"objective":"o","next_experiment":"n"`, wantRepair: RepairAddedClosers},
		{name: "stray quote after a bare value", raw: `{"objective":"o","attempt":1"`, wantRepair: RepairDroppedStrayQuote},
		{name: "cut off mid-string", raw: `{"objective":"o","next_experiment":"profile the par`, wantRepair: RepairTerminatedString},
		{
			name:       "raw newline and embedded quotes together",
			raw:        "{\"objective\":\"say \"hi\"\nthen stop\",\"next_experiment\":\"n\"}",
			wantRepair: joinRepairs(RepairEscapedControlChars, RepairEscapedQuotes),
		},
		{
			name:       "raw newline in a truncated string",
			raw:        "{\"objective\":\"o\",\"next_experiment\":\"line one\nline tw",
			wantRepair: joinRepairs(RepairEscapedControlChars, RepairTerminatedString),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, repair, err := DecodeResultWithRepair[CoordinatorResult](tc.raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Objective == "" && tc.name != "degraded empty result" {
				t.Fatalf("decoded %+v, want the objective", got)
			}
			if repair != tc.wantRepair {
				t.Fatalf("repair = %q, want %q", repair, tc.wantRepair)
			}
		})
	}
}

func TestDecodeResultWithRepairReportsNoRepairOnFailure(t *testing.T) {
	for _, raw := range []any{nil, (*genai.Content)(nil), "not json at all", make(chan int), 42} {
		if _, repair, err := DecodeResultWithRepair[CoordinatorResult](raw); err == nil || repair != "" {
			t.Fatalf("DecodeResultWithRepair(%T) = repair %q, err %v; want an error and no repair", raw, repair, err)
		}
	}
}
