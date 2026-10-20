package engine_test

import (
	"testing"

	"codelink/internal/testkit/harness"
)

func TestHealthzNeedsNoTokenAndV1Does(t *testing.T) {
	h := harness.Run(t, harness.Options{}, harness.OK)
	r, err := h.Request("GET", "/healthz", "", nil, false)
	if err != nil || r.Status != 200 {
		t.Fatalf("healthz: %v %d", err, r.Status)
	}
	r, _ = h.Request("GET", "/v1/models", "", nil, false)
	if r.Status != 401 || r.JSON()["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("no token: %d %s", r.Status, r.Text())
	}
	r, _ = h.Request("GET", "/v1/models", "", map[string]string{"x-api-key": "test-token-123"}, false)
	if r.Status != 200 {
		t.Fatalf("x-api-key: %d", r.Status)
	}
}

func TestModelsListsModelsAndAliasesInBothShapes(t *testing.T) {
	h := harness.Run(t, harness.Options{}, harness.OK)
	r, _ := h.Request("GET", "/v1/models", "", map[string]string{"anthropic-version": "2023-06-01"}, true)
	data := r.JSON()["data"].([]any)
	if len(data) != 6 || r.JSON()["first_id"] != "GLM" || r.JSON()["last_id"] != "haiku" {
		t.Fatalf("anthropic list: %s", r.Text())
	}
	r, _ = h.Request("GET", "/v1/models", "", nil, true)
	if r.JSON()["object"] != "list" || len(r.JSON()["data"].([]any)) != 6 {
		t.Fatalf("openai list: %s", r.Text())
	}
}
