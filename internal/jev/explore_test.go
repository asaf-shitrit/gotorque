package jev

import (
	"strings"
	"testing"
)

func TestFlagQuestionsNameEachOptionAndItsAliases(t *testing.T) {
	questions := FlagQuestions(map[string][]string{"--stream": {"-s"}, "--no-sort": nil})
	if len(questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(questions))
	}
	if q := questions["--stream"]; q.Type != "boolean" || !strings.Contains(q.Instructions, "--stream (also -s)") || !strings.Contains(q.Instructions, "`help`") {
		t.Errorf("--stream question = %+v", q)
	}
	if q := questions["--no-sort"]; !strings.Contains(q.Instructions, "option --no-sort change") {
		t.Errorf("--no-sort question = %+v", q)
	}
	state := FlagState("gron", "Usage: gron [OPTIONS]")
	if state["command"] != "gron" || state["help"] != "Usage: gron [OPTIONS]" || len(state) != 3 {
		t.Errorf("state = %v", state)
	}
}
