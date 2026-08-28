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
func DecodeResult[T any](raw any) (T, error) {
	var out T
	switch value := raw.(type) {
	case nil:
		return out, errors.New("decode agent output: no output")
	case string:
		return decodeText[T](value)
	case *genai.Content:
		if value == nil {
			return out, errors.New("decode agent output: empty content")
		}
		return decodeText[T](contentText(value))
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return out, fmt.Errorf("decode agent output %T: %w", raw, err)
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return out, fmt.Errorf("decode agent output: %w", err)
		}
		return out, nil
	}
}

func decodeText[T any](text string) (T, error) {
	var out T
	cleaned := RepairCommonMalformations(UnwrapJSONFence(text))
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		// Second chance: models frequently embed raw JSON documents inside
		// string values without escaping the quotes, which no strict parser
		// accepts.
		if repaired, changed := EscapeEmbeddedQuotes(cleaned); changed {
			repaired = RepairCommonMalformations(repaired)
			if err2 := json.Unmarshal([]byte(repaired), &out); err2 == nil {
				return out, nil
			}
		}
		for _, repaired := range RepairCandidates(cleaned) {
			repaired = RepairCommonMalformations(repaired)
			if err2 := json.Unmarshal([]byte(repaired), &out); err2 == nil {
				return out, nil
			}
		}
		return out, fmt.Errorf("decode agent output: %w (payload excerpt: %q)", err, excerptAround(cleaned, err))
	}
	return out, nil
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

func openJSONString(c, prev byte) (inString, valueString bool, nextPrev byte) {
	if c == '"' {
		inString = true
		valueString = prev == ':'
	}
	nextPrev = prev
	if !isJSONSpace(c) {
		nextPrev = c
	}
	return inString, valueString, nextPrev
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
// JSON string values. A quote ending a value string terminates it only when
// no embedded brace or bracket is still open and the next non-whitespace
// byte can legally follow a value; quotes that open in key position always
// terminate the key. It reports whether any rewrite happened.
func EscapeEmbeddedQuotes(text string) (string, bool) {
	var out []byte
	inString := false
	valueString := false // string opened right after ':'
	escaped := false
	changed := false
	embeddedDepth := 0
	prevSignificant := byte(0)
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inString {
			inString, valueString, prevSignificant = openJSONString(c, prevSignificant)
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
	if syntaxErr, ok := err.(*json.SyntaxError); ok {
		offset = int(syntaxErr.Offset)
	} else if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
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
	for _, key := range []string{"symbol", "name", "path", "location", "id", "title", "summary", "description", "value", "text"} {
		if raw, ok := object[key]; ok {
			if text, ok := raw.(string); ok && text != "" {
				return text
			}
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
	var candidates []string
	first, _ := RepairMissingClosers(text)
	candidates = append(candidates, first)
	// A quote that opens a string running to end of input is usually a
	// stray character the model appended after a bare value; dropping it
	// lets the structural closers apply.
	last := strings.LastIndex(text, "\"")
	if last >= 0 {
		withoutQuote := text[:last] + text[last+1:]
		again, _ := RepairMissingClosers(withoutQuote)
		candidates = append(candidates, again)
	}
	return candidates
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
