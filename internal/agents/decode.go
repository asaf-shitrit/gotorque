package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// DecodeResult converts raw workflow agent output into a typed result.
//
// Hosted models return judgment output in several shapes: a plain JSON
// object, JSON wrapped in Markdown code fences, JSON surrounded by prose,
// scalars where arrays were declared, or an unparsed genai.Content. The
// deterministic policy layer still validates everything downstream, so
// accepting these shapes here only removes brittle rejections of otherwise
// usable recommendations.
//
// A caller that must be able to tell a salvaged payload from the one the model
// sent uses DecodeResultWithRepair instead.
func DecodeResult[T any](raw any) (T, error) {
	out, _, err := DecodeResultWithRepair[T](raw)
	return out, err
}

// Repair names the rewrite that made a malformed payload parse. The empty
// Repair means the payload parsed as sent, after only the shape tolerances that
// cannot change what the model said: fence and prose unwrapping and trailing
// comma removal.
type Repair string

// The repairs, from the narrowest rewrite to the most speculative. A repair that
// needed two rewrites names both, joined by repairJoin.
const (
	RepairEscapedControlChars Repair = "escaped raw control characters inside a string"
	RepairEscapedQuotes       Repair = "escaped unescaped quotes inside a string"
	RepairAddedClosers        Repair = "added missing closing brackets"
	RepairDroppedStrayQuote   Repair = "dropped a stray trailing quote"
	RepairTerminatedString    Repair = "closed a string cut off mid-value (truncated output)"

	repairJoin = ", then "
)

// DecodeResultWithRepair is DecodeResult that also reports which repair, if any,
// the payload needed before it parsed.
//
// The repairs are deliberate: each turns a lost cycle into an answer the
// deterministic half still judges. But a repaired value used to be
// indistinguishable from an intended one. An optimizer patch cut off at the
// output-token cap came through terminateOpenString, NormalizeUnifiedDiff then
// recomputed its hunk counts, and the candidate record read exactly like a
// patch the model meant to send. Reporting the repair lets the campaign record
// the difference; it never changes the decoded value or any verdict.
func DecodeResultWithRepair[T any](raw any) (T, Repair, error) {
	var out T
	switch value := raw.(type) {
	case nil:
		return out, "", errors.New("decode agent output: no output")
	case string:
		return decodeText[T](value)
	case *genai.Content:
		if value == nil {
			return out, "", errors.New("decode agent output: empty content")
		}
		return decodeText[T](contentText(value))
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return out, "", fmt.Errorf("decode agent output %T: %w", raw, err)
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return out, "", fmt.Errorf("decode agent output: %w", err)
		}
		return out, "", nil
	}
}

func decodeText[T any](text string) (T, Repair, error) {
	var out T
	cleaned := RepairCommonMalformations(UnwrapJSONFence(text))
	err := json.Unmarshal([]byte(cleaned), &out)
	if err == nil {
		return out, "", nil
	}
	// Each repair below has exactly one sane reading of a mistake models
	// actually make. A failed attempt can leave partial data behind, so every
	// candidate decodes into a fresh value and the first one that parses wins.
	for _, attempt := range repairAttempts(cleaned) {
		var repaired T
		if json.Unmarshal([]byte(attempt.text), &repaired) == nil {
			return repaired, attempt.repair, nil
		}
	}
	return out, "", fmt.Errorf("decode agent output: %w (payload excerpt: %q)", err, excerptAround(cleaned, err))
}

// repairAttempt is one repaired form of a payload and the rewrite that made it.
type repairAttempt struct {
	text   string
	repair Repair
}

// repairAttempts returns repaired forms of a payload that failed to parse,
// ordered from the narrowest rewrite to the most speculative one. Retrying the
// model is not the alternative to repairing here: the retry loop that wraps the
// model has already spent its attempts by the time this code runs, so a payload
// left unrepaired costs the whole campaign cycle, not one more request.
func repairAttempts(cleaned string) []repairAttempt {
	var attempts []repairAttempt
	appendIf := func(text string, changed bool, repair Repair) {
		if changed {
			attempts = append(attempts, repairAttempt{text: RepairCommonMalformations(text), repair: repair})
		}
	}
	controls, controlsChanged := EscapeRawControlChars(cleaned)
	appendIf(controls, controlsChanged, RepairEscapedControlChars)
	quoted, quotedChanged := EscapeEmbeddedQuotes(cleaned)
	appendIf(quoted, quotedChanged, RepairEscapedQuotes)
	if controlsChanged {
		// Pasting a diff verbatim breaks the newlines and the quotes at once;
		// neither single repair parses on its own.
		both, bothChanged := EscapeEmbeddedQuotes(controls)
		appendIf(both, bothChanged, joinRepairs(RepairEscapedControlChars, RepairEscapedQuotes))
	}
	attempts = append(attempts, structuralRepairs(cleaned, "")...)
	if controlsChanged {
		attempts = append(attempts, structuralRepairs(controls, RepairEscapedControlChars)...)
	}
	return attempts
}

// structuralRepairs returns the closer and truncation repairs of text, each
// named after prior, the rewrite text already carries.
func structuralRepairs(text string, prior Repair) []repairAttempt {
	repaired := repairCandidates(text)
	for i := range repaired {
		repaired[i].text = RepairCommonMalformations(repaired[i].text)
		repaired[i].repair = joinRepairs(prior, repaired[i].repair)
	}
	return repaired
}

// joinRepairs names a repair applied on top of another one.
func joinRepairs(first, then Repair) Repair {
	if first == "" {
		return then
	}
	return first + repairJoin + then
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func skipJSONSpace(s string, i int) int {
	for i < len(s) && isJSONSpace(s[i]) {
		i++
	}
	return i
}

func emptyOrNull(trimmed []byte) bool {
	return len(trimmed) == 0 || string(trimmed) == "null"
}

// scanJSONString updates escape state while inside a JSON string and reports
// whether the string is still open after consuming c.
func scanJSONString(c byte, escaped *bool) bool {
	if *escaped {
		*escaped = false
		return true
	}
	if c == '\\' {
		*escaped = true
		return true
	}
	return c != '"'
}

func openJSONString(c, prev byte, inArray bool) (inString, valueString bool, nextPrev byte) {
	if c == '"' {
		inString = true
		// A string opened after ':' is an object value, and one opened after
		// '[' or ',' inside an array is an element. Both carry model-authored
		// text — for the optimizer, one diff line per element — and so may hold
		// unescaped quotes. Keys never do.
		valueString = prev == ':' || (inArray && (prev == '[' || prev == ','))
	}
	nextPrev = prev
	if !isJSONSpace(c) {
		nextPrev = c
	}
	return inString, valueString, nextPrev
}

// trackContainer maintains the stack of open JSON containers so a string can
// be attributed to the array or object that encloses it.
func trackContainer(stack []byte, c byte) []byte {
	switch c {
	case '{', '[':
		return append(stack, c)
	case '}', ']':
		if len(stack) > 0 {
			return stack[:len(stack)-1]
		}
	}
	return stack
}

func innermostIsArray(stack []byte) bool {
	return len(stack) > 0 && stack[len(stack)-1] == '['
}

func quoteTerminatesValue(s string, i, embeddedDepth int) bool {
	j := skipJSONSpace(s, i+1)
	if j >= len(s) {
		return true
	}
	if embeddedDepth > 0 {
		return false
	}
	switch s[j] {
	case ',', '}', ']':
		return true
	}
	return false
}

func bumpEmbeddedDepth(c byte, depth int) int {
	switch c {
	case '{', '[':
		return depth + 1
	case '}', ']':
		if depth > 0 {
			return depth - 1
		}
	}
	return depth
}

func escapeQuote(c byte, text string, i, depth int, valueString bool, out []byte) ([]byte, bool, bool) {
	if !valueString {
		return append(out, c), false, false
	}
	if quoteTerminatesValue(text, i, depth) {
		return append(out, '"'), false, false
	}
	return append(out, '\\', '"'), true, true
}

func trailingComma(text string, i int) bool {
	j := skipJSONSpace(text, i+1)
	return j < len(text) && (text[j] == '}' || text[j] == ']')
}

// EscapeEmbeddedQuotes rewrites unescaped double quotes that appear inside
// JSON string values and array elements. A quote ending such a string
// terminates it only when no embedded brace or bracket is still open and the
// next non-whitespace byte can legally follow a value; quotes that open in key
// position always terminate the key. It reports whether any rewrite happened.
func EscapeEmbeddedQuotes(text string) (string, bool) {
	var out []byte
	inString := false
	valueString := false // string holding model text rather than a key
	escaped := false
	changed := false
	embeddedDepth := 0
	prevSignificant := byte(0)
	var containers []byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inString {
			containers = trackContainer(containers, c)
			inString, valueString, prevSignificant = openJSONString(c, prevSignificant, innermostIsArray(containers))
			out = append(out, c)
			continue
		}
		if escaped {
			escaped = false
			out = append(out, c)
			continue
		}
		if c == '\\' {
			escaped = true
			out = append(out, c)
			continue
		}
		if c == '"' {
			var extra bool
			out, inString, extra = escapeQuote(c, text, i, embeddedDepth, valueString, out)
			changed = changed || extra
			continue
		}
		embeddedDepth = bumpEmbeddedDepth(c, embeddedDepth)
		out = append(out, c)
	}
	return string(out), changed
}

// EscapeRawControlChars escapes literal control characters found inside JSON
// string values. A model asked to put a multi-line document into a string —
// a unified diff above all — routinely writes the line breaks and tabs raw.
// No strict parser accepts those, and none of the other repairs here help:
// they assume the payload still tokenizes. The rewrite has one reading, since
// a raw control byte inside a string is never valid JSON. It reports whether
// any rewrite happened.
func EscapeRawControlChars(text string) (string, bool) {
	var out []byte
	inString := false
	escaped := false
	changed := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inString {
			if c == '"' {
				inString = true
				escaped = false
			}
			out = append(out, c)
			continue
		}
		if escaped {
			escaped = false
			out = append(out, c)
			continue
		}
		if c == '\\' {
			escaped = true
			out = append(out, c)
			continue
		}
		if replacement, ok := jsonControlEscape(c); ok {
			out = append(out, replacement...)
			changed = true
			continue
		}
		if c == '"' {
			inString = false
		}
		out = append(out, c)
	}
	return string(out), changed
}

// jsonControlEscape renders a control byte as the escape sequence JSON
// requires, reporting false for bytes that need no escaping.
func jsonControlEscape(c byte) (string, bool) {
	switch c {
	case '\n':
		return `\n`, true
	case '\r':
		return `\r`, true
	case '\t':
		return `\t`, true
	}
	if c < 0x20 {
		return fmt.Sprintf(`\u%04x`, c), true
	}
	return "", false
}

// RepairCommonMalformations removes trailing commas before object or
// array closers, a frequent model mistake that has exactly one sane
// interpretation. String contents are left untouched.
func RepairCommonMalformations(text string) string {
	var out []byte
	inString := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			out = append(out, c)
			inString = scanJSONString(c, &escaped)
			continue
		}
		if c == '"' {
			inString = true
		} else if c == ',' && trailingComma(text, i) {
			continue // drop the trailing comma
		}
		out = append(out, c)
	}
	return string(out)
}

// excerptAround returns a short window of the payload around the JSON
// error offset so malformed model output can be diagnosed from logs
// without persisting the full payload.
func excerptAround(text string, err error) string {
	const width = 160
	offset := -1
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) {
		offset = int(syntaxErr.Offset)
	} else if errors.As(err, &typeErr) {
		offset = int(typeErr.Offset)
	}
	if offset < 0 || offset > len(text) {
		if len(text) > width {
			return text[:width]
		}
		return text
	}
	start := offset - width/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}

func contentText(content *genai.Content) string {
	var b []byte
	for _, part := range content.Parts {
		if part == nil || part.Thought || part.Text == "" {
			continue
		}
		b = append(b, part.Text...)
	}
	return string(b)
}

// flexStrings decodes either a JSON string or a JSON array of strings.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if values, done, err := flexStringsFromScalar(trimmed); done {
		if err != nil {
			return err
		}
		*f = values
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return err
	}
	values := make([]string, 0, len(list))
	for _, element := range list {
		s, err := flexStringElement(element)
		if err != nil {
			return err
		}
		values = append(values, s)
	}
	*f = values
	return nil
}

func flexStringsFromScalar(trimmed []byte) (flexStrings, bool, error) {
	if len(trimmed) == 0 {
		return nil, true, nil
	}
	switch trimmed[0] {
	case '"':
		var single string
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, true, err
		}
		if single == "" {
			return nil, true, nil
		}
		return flexStrings{single}, true, nil
	case '{':
		// A lone object collapses to its identifying string field, or its
		// compact JSON when nothing recognizable exists.
		var object map[string]any
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, true, err
		}
		if extracted := identifyingString(object); extracted != "" {
			return flexStrings{extracted}, true, nil
		}
		return flexStrings{string(trimmed)}, true, nil
	}
	return nil, false, nil
}

func flexStringElement(element json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(element, &text); err == nil {
		return text, nil
	}
	// Objects like {"symbol": ..., "role": ...} collapse to their
	// most identifying string field.
	var object map[string]any
	if err := json.Unmarshal(element, &object); err == nil {
		if extracted := identifyingString(object); extracted != "" {
			return extracted, nil
		}
	}
	compact, err := json.Marshal(element)
	if err != nil {
		return "", err
	}
	return string(compact), nil
}

// identifyingString picks the most descriptive scalar from an object
// produced by a model for what our schema declared as a plain string.
func identifyingString(object map[string]any) string {
	return firstString(object, "symbol", "name", "path", "location", "id", "title", "summary", "description", "value", "text")
}

// firstString returns the first non-empty string value stored under keys,
// checked in order, or "" when none qualifies.
func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}

// flexBool decodes a JSON boolean, or common boolean spellings sent as strings.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		switch text {
		case "true", "True", "TRUE", "yes", "Yes", "proceed":
			*b = true
		default:
			*b = false
		}
		return nil
	}
	var value bool
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return err
	}
	*b = flexBool(value)
	return nil
}

func trimSpaceBytes(data []byte) []byte {
	start := 0
	for start < len(data) && isJSONSpace(data[start]) {
		start++
	}
	end := len(data)
	for end > start && isJSONSpace(data[end-1]) {
		end--
	}
	return data[start:end]
}

// RepairCandidates returns plausible repaired forms of a malformed JSON
// payload: the standard closer completion plus, when the payload ends in
// an unterminated string, a variant that drops that stray opening quote
// before completing closers. Callers try each until one parses.
func RepairCandidates(text string) []string {
	attempts := repairCandidates(text)
	candidates := make([]string, len(attempts))
	for i, attempt := range attempts {
		candidates[i] = attempt.text
	}
	return candidates
}

// repairCandidates is RepairCandidates with each candidate named by the repair
// that produced it.
func repairCandidates(text string) []repairAttempt {
	first, _ := RepairMissingClosers(text)
	candidates := []repairAttempt{{text: first, repair: RepairAddedClosers}}
	// A quote that opens a string running to end of input is usually a
	// stray character the model appended after a bare value; dropping it
	// lets the structural closers apply.
	if last := strings.LastIndex(text, "\""); last >= 0 {
		again, _ := RepairMissingClosers(text[:last] + text[last+1:])
		candidates = append(candidates, repairAttempt{text: again, repair: RepairDroppedStrayQuote})
	}
	// A response cut off at the output-token cap ends in the middle of a
	// string value, which blocks the closer completion above. Terminating that
	// string turns a lost cycle into a truncated recommendation that patch
	// validation and the build gate still get to judge on its merits.
	if terminated, ok := terminateOpenString(text); ok {
		closed, _ := RepairMissingClosers(terminated)
		candidates = append(candidates, repairAttempt{text: closed, repair: RepairTerminatedString})
	}
	return candidates
}

// terminateOpenString closes a string value left open at the end of a
// truncated payload, reporting false when the payload does not end inside one.
func terminateOpenString(text string) (string, bool) {
	inString := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			inString = scanJSONString(c, &escaped)
			continue
		}
		if c == '"' {
			inString = true
			escaped = false
		}
	}
	if !inString {
		return text, false
	}
	if escaped {
		// The cut landed just after a backslash, which would otherwise
		// swallow the quote being appended.
		text = text[:len(text)-1]
	}
	return text + `"`, true
}

// RepairMissingClosers inserts structurally required closing braces and
// brackets that the model omitted, e.g. {"a":[{"b":"c"]} missing the
// object closer before the array closer. Only applied after a plain
// parse fails; string contents are preserved verbatim.
func RepairMissingClosers(text string) (string, bool) {
	var out []byte
	var stack []byte
	inString := false
	escaped := false
	changed := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			out = append(out, c)
			inString = scanJSONString(c, &escaped)
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			out, stack, changed = closeMismatched(out, stack, c, changed)
		}
		out = append(out, c)
	}
	return finishClosers(out, stack, inString, changed)
}

func closeMismatched(out, stack []byte, c byte, changed bool) ([]byte, []byte, bool) {
	for len(stack) > 0 && stack[len(stack)-1] != c {
		// Close the inner structure that was left open.
		out = append(out, stack[len(stack)-1])
		stack = stack[:len(stack)-1]
		changed = true
	}
	if len(stack) > 0 {
		stack = stack[:len(stack)-1]
	}
	return out, stack, changed
}

func finishClosers(out, stack []byte, inString, changed bool) (string, bool) {
	if !inString {
		for i := len(stack) - 1; i >= 0; i-- {
			out = append(out, stack[i])
			changed = true
		}
	}
	return string(out), changed
}
