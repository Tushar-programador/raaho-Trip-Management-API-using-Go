package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The status line is already sent, so an encode error (client gone) has
	// nowhere useful to go.
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON strictly decodes a single JSON object into dst, a pointer to a
// flat struct: the body must be application/json, fit the limit set by
// middleware.MaxBodyBytes, and contain exactly one object whose keys are
// unique and match dst's json tags exactly.
func decodeJSON(r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return decodeError(err)
	}
	if err := checkObjectKeys(body, jsonFieldNames(dst)); err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return decodeError(err)
	}
	return nil
}

// checkObjectKeys walks the top level of body. encoding/json on its own
// matches keys case-insensitively (even with DisallowUnknownFields), lets a
// repeated key silently override an earlier one, and accepts a bare null, so
// {"Status":"X"} or {"status":"A","status":"B"} would otherwise slip through.
func checkObjectKeys(body []byte, allowed map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return decodeError(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return invalidJSON("Request body must be a JSON object")
	}

	seen := make(map[string]bool, len(allowed))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return decodeError(err)
		}
		key, ok := tok.(string)
		if !ok {
			return invalidJSON("Request body contains malformed JSON")
		}
		if !allowed[key] {
			return invalidJSON(fmt.Sprintf("Request body contains unknown field %q", key))
		}
		if seen[key] {
			return invalidJSON(fmt.Sprintf("Request body contains duplicate field %q", key))
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return decodeError(err)
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return decodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalidJSON("Request body must contain a single JSON object")
	}
	return nil
}

// jsonFieldNames returns the JSON keys a struct (or pointer to one) accepts.
func jsonFieldNames(v any) map[string]bool {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	names := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		names[name] = true
	}
	return names
}

func decodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)
	switch {
	case errors.As(err, &maxErr):
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge, Message: fmt.Sprintf("Request body must not exceed %d bytes", maxErr.Limit)}
	case errors.Is(err, io.EOF):
		return invalidJSON("Request body must not be empty")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return invalidJSON("Request body contains malformed JSON")
	case errors.As(err, &typeErr):
		if typeErr.Field == "" {
			return invalidJSON("Request body must be a JSON object")
		}
		return invalidJSON(fmt.Sprintf("Field %q must be of type %s", typeErr.Field, typeErr.Type))
	default:
		return invalidJSON("Request body could not be decoded")
	}
}

func invalidJSON(msg string) error {
	return &apiError{Status: http.StatusBadRequest, Code: CodeInvalidJSON, Message: msg}
}
