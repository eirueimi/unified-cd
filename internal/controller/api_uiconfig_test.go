package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleUIConfig_StderrPlain(t *testing.T) {
	for _, want := range []bool{true, false} {
		s := &Server{cfg: Config{StderrPlain: want}}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
		rec := httptest.NewRecorder()

		s.handleUIConfig(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body struct {
			StderrPlain bool `json:"stderrPlain"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.StderrPlain != want {
			t.Fatalf("stderrPlain = %v, want %v", body.StderrPlain, want)
		}
	}
}
