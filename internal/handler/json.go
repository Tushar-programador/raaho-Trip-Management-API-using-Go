package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
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

// decodeJSON strictly decodes a single JSON object into dst: the body must be
// application/json, contain exactly one value, use only known fields and fit
// the body limit set by middleware.MaxBodyBytes.
func decodeJSON(r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return decodeError(err)
		}
		return invalidJSON("Request body must contain a single JSON object")
	}
	return nil
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
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json exposes no typed error for unknown fields.
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return invalidJSON("Request body contains unknown field " + field)
	default:
		return invalidJSON("Request body could not be decoded")
	}
}

func invalidJSON(msg string) error {
	return &apiError{Status: http.StatusBadRequest, Code: CodeInvalidJSON, Message: msg}
}
