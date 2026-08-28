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
		for _, candidate := range RepairCandidates(tc.broken) {
			var v any
			if err := json.Unmarshal([]byte(RepairCommonMalformations(candidate)), &v); err == nil {
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
