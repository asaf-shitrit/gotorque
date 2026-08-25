package agents

import (
	"encoding/json"
	"errors"
	"strings"
	"fmt"

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
			if c == '"' {
				inString = true
				valueString = prevSignificant == ':'
			}
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prevSignificant = c
			}
			out = append(out, c)
			continue
		}
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			if !valueString {
				inString = false
				out = append(out, c)
				continue
			}
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
				j++
			}
			terminator := j >= len(text)
			if !terminator && embeddedDepth <= 0 {
				switch text[j] {
				case ',', '}', ']':
					terminator = true
				}
			}
			if terminator {
				inString = false
			} else {
				changed = true
				out = append(out, '\\')
			}
			out = append(out, '"')
			continue
		case c == '{' || c == '[':
			embeddedDepth++
		case c == '}' || c == ']':
			if embeddedDepth > 0 {
				embeddedDepth--
			}
		}
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
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ',':
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
				j++
			}
			if j < len(text) && (text[j] == '}' || text[j] == ']') {
				continue // drop the trailing comma
			}
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
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var single string
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return err
		}
		if single == "" {
			*f = nil
			return nil
		}
		*f = []string{single}
		return nil
	}
	if trimmed[0] == '{' {
		// A lone object collapses to its identifying string field, or its
		// compact JSON when nothing recognizable exists.
		var object map[string]any
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		if extracted := identifyingString(object); extracted != "" {
			*f = []string{extracted}
			return nil
		}
		*f = []string{string(trimmed)}
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return err
	}
	values := make([]string, 0, len(list))
	for _, element := range list {
		var text string
		if err := json.Unmarshal(element, &text); err == nil {
			values = append(values, text)
			continue
		}
		// Objects like {"symbol": ..., "role": ...} collapse to their
		// most identifying string field.
		var object map[string]any
		if err := json.Unmarshal(element, &object); err == nil {
			if extracted := identifyingString(object); extracted != "" {
				values = append(values, extracted)
				continue
			}
		}
		compact, err := json.Marshal(element)
		if err != nil {
			return err
		}
		values = append(values, string(compact))
	}
	*f = values
	return nil
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
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\t' || data[end-1] == '\n' || data[end-1] == '\r') {
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
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
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
			for len(stack) > 0 && stack[len(stack)-1] != c {
				// Close the inner structure that was left open.
				out = append(out, stack[len(stack)-1])
				stack = stack[:len(stack)-1]
				changed = true
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		out = append(out, c)
	}
	if !inString {
		for i := len(stack) - 1; i >= 0; i-- {
			out = append(out, stack[i])
			changed = true
		}
	}
	return string(out), changed
}
