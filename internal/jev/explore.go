package jev

import (
	"fmt"
	"strings"
)

const exploreContext = "A Go command-line program and its own help text. Deciding which of its options to exercise when profiling ordinary use."

// ModeFloor is the probability at or above which an option counts as a
// processing mode. Each option is its own question judged on its own, so no
// baseline is needed: at 0.5 Jev leans yes.
const ModeFloor = 0.5

// FlagQuestions asks, for every flag in one request, whether it makes the
// program do different work on the same input. That is the one judgment the
// explorer needs. An earlier wording also asked whether a typical user passes
// the option, and Jev answered that honestly: most users pass none, so on
// gron every option but --ungron fell below one half, the one that fails on
// JSON input. Questions ids never reach the model, so each instruction names
// its option, with any aliases.
func FlagQuestions(flags map[string][]string) map[string]Question {
	questions := make(map[string]Question, len(flags))
	for name, aliases := range flags {
		spelled := name
		if len(aliases) > 0 {
			spelled = fmt.Sprintf("%s (also %s)", name, strings.Join(aliases, ", "))
		}
		questions[name] = boolean(
			"Does the option "+spelled+" change how the program described in `help` processes its input or formats its output?",
			"The option selects a processing mode, an input format, or an output format, so the program does different work on the same input.",
			"The option prints help or version information, turns on debugging or profiling, configures network access such as proxies or TLS, or changes nothing about how the input is processed.")
	}
	return questions
}

// FlagState is the program as its user meets it: its command and its own help
// output. Nothing else, so the judgment rests on what the program says it does.
func FlagState(command, help string) map[string]string {
	return map[string]string{"context": exploreContext, "command": command, "help": help}
}
