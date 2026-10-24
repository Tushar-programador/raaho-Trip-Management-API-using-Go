package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoggerRecordsClientID(t *testing.T) {
	for _, clientID := range []string{"", "0192f3a4-5b6c-7d8e-9f01-23456789abcd"} {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		h := Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if clientID != "" {
				SetClientID(r.Context(), clientID)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/trips", nil))

		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, buf.String())
		}
		got, present := line["client_id"]
		if clientID == "" && present {
			t.Errorf("client_id logged for an unauthenticated request: %v", got)
		}
		if clientID != "" && got != clientID {
			t.Errorf("client_id = %v, want %s", got, clientID)
		}
		if line["status"] != float64(http.StatusNoContent) {
			t.Errorf("status = %v", line["status"])
		}
	}
}

func TestSetClientIDOutsideLoggerIsNoOp(t *testing.T) {
	SetClientID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "x") // must not panic
}
