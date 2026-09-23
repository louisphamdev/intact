package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
)

// setModel returns body with its top-level "model" value replaced by model.
//
// It splices the bytes instead of decoding and re-encoding, so every other byte
// of the caller's request (key order, number formatting, whitespace) reaches the
// upstream as sent. A body that is not a JSON object, or has no top-level model,
// comes back unchanged with ok false.
func setModel(body []byte, model string) (out []byte, ok bool) {
	start, end, err := topLevelValue(body, "model")
	if err != nil {
		return body, false
	}
	val, err := json.Marshal(model)
	if err != nil {
		return body, false
	}
	out = make([]byte, 0, len(body)-(end-start)+len(val))
	out = append(out, body[:start]...)
	out = append(out, val...)
	out = append(out, body[end:]...)
	return out, true
}

// topLevelCount counts how often key appears at the top level of a JSON object.
// JSON allows a repeated key and readers disagree on which one wins, so the
// model path refuses a body that names the model twice.
func topLevelCount(body []byte, key string) int {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return 0
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0
	}
	n := 0
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return n
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return n
		}
		if k, _ := kt.(string); k == key {
			n++
		}
	}
	return n
}

// topLevelValue finds the byte range of the value of key in a JSON object,
// looking at the top level only (a "model" nested in a message is not it).
func topLevelValue(body []byte, key string) (start, end int, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, errors.New("not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		k, _ := kt.(string)
		// The offset sits just after the key; the value starts past the colon.
		keyEnd := int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, err
		}
		if k != key {
			continue
		}
		valEnd := int(dec.InputOffset())
		i := bytes.IndexByte(body[keyEnd:valEnd], ':')
		if i < 0 {
			return 0, 0, errors.New("malformed object")
		}
		valStart := keyEnd + i + 1
		for valStart < valEnd && isSpace(body[valStart]) {
			valStart++
		}
		return valStart, valEnd, nil
	}
	return 0, 0, errors.New("key not found")
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
