package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/share"
)

// gatewayStub is a go-llm-proxy: models A and B for the key "good", its
// capabilities with the given defaults.
func gatewayStub(t *testing.T, defaults map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			io.WriteString(w, `{"data":[{"id":"A","max_model_len":262144},{"id":"B","max_model_len":1048576}]}`)
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(map[string]any{"service": "go-llm-proxy", "web_search": "tavily", "documents": "paddleocr", "defaults": defaults})
		case r.URL.Path == "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "image_url") {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"text only"}}`)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"logprobs":{"content":[{"token":"ok","logprob":-0.1,"top_logprobs":[{"token":"ok","logprob":-0.1}]}]}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fresh is the default config with the built-in gateway on the stub.
func fresh(t *testing.T, srv *httptest.Server) config.Config {
	c := config.Default()
	c.Backends[0].BaseURL = srv.URL + "/v1"
	c.Port = freePort(t)
	return c
}

func TestKeyOnlySetupIsTheBuiltInGatewayWithoutAKey(t *testing.T) {
	c := config.Default()
	if b, ok := KeyOnlySetup(&c); !ok || b.ID != config.DefaultGatewayID || JoinURL(b) != "https://llm.eidonix.com/join" {
		t.Fatalf("%+v %v", b, ok)
	}
	c.Backends[0].APIKey = "k"
	if _, ok := KeyOnlySetup(&c); ok {
		t.Error("has a key")
	}
	c = config.Default()
	AddBackend(&c)
	if _, ok := KeyOnlySetup(&c); ok {
		t.Error("another provider: manual setup")
	}
	c = base()
	SetRole(&c, SlotMain, ch("a", "M"), nil)
	if _, ok := KeyOnlySetup(&c); ok {
		t.Error("configured")
	}
}

func TestPastingAKeyEnablesTheGatewaysModelsWithItsDefaultsAndSaves(t *testing.T) {
	a := newTestApp(t)
	srv := gatewayStub(t, map[string]string{"main": "B", "opus": "A", "decision": "B", "vision": "missing"})
	r := a.ApplyKeys(context.Background(), fresh(t, srv), map[string]string{config.DefaultGatewayID: "  good \n"}, true)
	if !r.OK || r.Save == nil || !r.Save.OK {
		t.Fatalf("%+v", r)
	}
	c := a.Config()
	if c.Backends[0].APIKey != "good" || len(c.Models) != 2 {
		t.Fatalf("%+v", c)
	}
	if c.Roles != (config.Roles{Main: "B", Opus: "A", Decision: "B"}) {
		t.Errorf("roles %+v", c.Roles)
	}
	b, _ := c.Model("B")
	if *b.ContextWindow != 1_048_576 || b.Logprobs == nil || !*b.Logprobs || b.Vision == nil || *b.Vision {
		t.Errorf("probed facts: %+v", b)
	}
	if !a.Running() || r.Save.Status.Kind != Running {
		t.Errorf("status %+v", r.Save.Status)
	}
	// Web search and documents now go through the gateway on their own.
	if r.View.SearchValue != "auto" || r.View.DocumentsValue != "auto" || r.View.Search[1].Label != "Through llm.eidonix.com (automatic)" {
		t.Errorf("view %+v %+v", r.View.Search, r.View.DocumentsValue)
	}
}

func TestWithoutUsableDefaultsTheFirstModelIsMain(t *testing.T) {
	a := newTestApp(t)
	srv := gatewayStub(t, map[string]string{"main": "gone", "small": "A"})
	r := a.ApplyKeys(context.Background(), fresh(t, srv), map[string]string{config.DefaultGatewayID: "good"}, false)
	if !r.OK || r.Save != nil {
		t.Fatalf("%+v", r)
	}
	if r.View.Draft.Roles != (config.Roles{Main: "A", Small: "A"}) {
		t.Errorf("roles %+v", r.View.Draft.Roles)
	}
	if _, err := os.Stat(a.Path); err == nil {
		if back, _ := LoadSettings(a.Path); back.Configured() {
			t.Error("not saved")
		}
	}
}

func TestKeyErrorsAreInPlainWords(t *testing.T) {
	a := newTestApp(t)
	srv := gatewayStub(t, nil)
	bad := a.ApplyKeys(context.Background(), fresh(t, srv), map[string]string{config.DefaultGatewayID: "wrong"}, true)
	if bad.OK || bad.Save != nil || !strings.Contains(bad.Errors[config.DefaultGatewayID], "That key wasn't accepted") {
		t.Errorf("%+v", bad.Errors)
	}
	if a.Configured() {
		t.Error("nothing saved")
	}
	empty := a.ApplyKeys(context.Background(), fresh(t, srv), map[string]string{config.DefaultGatewayID: " "}, true)
	if empty.Errors[config.DefaultGatewayID] != "Paste your API key." {
		t.Errorf("%+v", empty.Errors)
	}
	gone := config.Default()
	gone.Backends[0].BaseURL = "http://127.0.0.1:" + strconv.Itoa(freePort(t)) + "/v1"
	down := a.ApplyKeys(context.Background(), gone, map[string]string{config.DefaultGatewayID: "good"}, true)
	if !strings.HasPrefix(down.Errors[config.DefaultGatewayID], "Can't reach 127.0.0.1:") {
		t.Errorf("%+v", down.Errors)
	}
	b := config.Backend{ID: "x", BaseURL: "https://llm.eidonix.com/v1"}
	for err, want := range map[string]string{"HTTP 403: no": "wasn't accepted", "HTTP 502: bad": "isn't answering right now (HTTP 502)",
		"HTTP 404: no": "answered HTTP 404", "dial tcp: no such host": "Can't reach llm.eidonix.com"} {
		if got := KeyError(b, core.Probe{Error: err}); !strings.Contains(got, want) {
			t.Errorf("%s: %q", err, got)
		}
	}
	if got := KeyError(b, core.Probe{Reachable: true}); got != "llm.eidonix.com has no models for this key." {
		t.Error(got)
	}
}

func TestKeysForOtherProvidersAreOptionalAndNotTested(t *testing.T) {
	a := newTestApp(t)
	c := base()
	r := a.ApplyKeys(context.Background(), c, map[string]string{"a": "sk-a", "b": ""}, false)
	if !r.OK || r.View.Draft.Backends[0].APIKey != "sk-a" || r.View.Draft.Backends[1].APIKey != "" {
		t.Errorf("%+v", r)
	}
}

func TestSearchChoices(t *testing.T) {
	c := config.Default()
	c.Backends = append(c.Backends, config.Backend{ID: "vllm", Kind: config.OpenAICompatible, BaseURL: "http://v/v1"},
		config.Backend{ID: "gw2", Kind: config.OpenAICompatible, BaseURL: "http://g/v1", APIKey: "k"})
	probes := map[string]core.Probe{"gw2": {Reachable: true, Gateway: &core.Gateway{WebSearch: true}}}
	values := func(os []Option) []string {
		var out []string
		for _, o := range os {
			out = append(out, o.Value)
		}
		return out
	}
	// No keyed Go-LLM-Proxy: nothing automatic; the built-in one and a
	// provider whose probe reported web search are offered.
	if SearchChoice(&c) != "off" || !reflect.DeepEqual(values(SearchOptions(&c, probes)), []string{"off", "via:llm.eidonix.com", "via:gw2", "tavily", "brave"}) {
		t.Errorf("%s %v", SearchChoice(&c), SearchOptions(&c, probes))
	}
	c.Backends[0].APIKey = "k"
	if SearchChoice(&c) != "auto" || !reflect.DeepEqual(values(SearchOptions(&c, probes)), []string{"off", "auto", "via:gw2", "tavily", "brave"}) {
		t.Errorf("%s %v", SearchChoice(&c), SearchOptions(&c, probes))
	}
	SetSearchChoice(&c, "off")
	if !c.WebSearch.Off || SearchChoice(&c) != "off" {
		t.Error("off")
	}
	if _, ok := c.SearchVia(); ok {
		t.Error("off is really off with a keyed gateway")
	}
	SetSearchChoice(&c, "via:gw2")
	if b, ok := c.SearchVia(); !ok || b.ID != "gw2" || c.WebSearch.Off || SearchChoice(&c) != "via:gw2" {
		t.Errorf("%+v", c.WebSearch)
	}
	c.WebSearch.APIKey = "tvly"
	SetSearchChoice(&c, "tavily")
	if c.WebSearch.Provider != config.Tavily || c.WebSearch.Via != "" || c.WebSearch.APIKey != "tvly" {
		t.Errorf("%+v", c.WebSearch)
	}
	SetSearchChoice(&c, "auto")
	if c.WebSearch.Provider != "" || SearchChoice(&c) != "auto" {
		t.Errorf("%+v", c.WebSearch)
	}
}

func TestDocumentsChoices(t *testing.T) {
	c := config.Default()
	if DocumentsChoice(&c) != "local" {
		t.Error("no gateway key: local")
	}
	c.Backends[0].APIKey = "k"
	if DocumentsChoice(&c) != "auto" || DocumentsOptions(&c, nil)[0].Label != "Through llm.eidonix.com (automatic)" {
		t.Errorf("%v", DocumentsOptions(&c, nil))
	}
	SetDocumentsChoice(&c, "local")
	if _, ok := c.DocumentsVia(); ok || DocumentsChoice(&c) != "local" || !c.Documents.LocalOnly {
		t.Errorf("local only: %+v", c.Documents)
	}
	SetDocumentsChoice(&c, "paddle")
	if c.Documents.PaddleOCR != "http://" || DocumentsChoice(&c) != "paddle" || c.Documents.LocalOnly {
		t.Errorf("paddle: %+v", c.Documents)
	}
	if p := strings.Join(Problems(&c), " "); !strings.Contains(p, "PaddleOCR") {
		t.Error("an unfinished URL is a problem")
	}
	c.Documents.PaddleOCR = "http://ocr:8002/layout-parsing"
	if _, ok := c.DocumentsVia(); ok {
		t.Error("own PaddleOCR")
	}
	SetDocumentsChoice(&c, "via:llm.eidonix.com")
	if b, ok := c.DocumentsVia(); !ok || b.ID != config.DefaultGatewayID || DocumentsChoice(&c) != "via:llm.eidonix.com" {
		t.Errorf("%+v", c.Documents)
	}
	SetDocumentsChoice(&c, "auto")
	if c.Documents.PaddleOCR != "" || DocumentsChoice(&c) != "auto" {
		t.Errorf("%+v", c.Documents)
	}
}

func TestViaFollowsProviderRenamesAndRemovalsAndNeedsAKey(t *testing.T) {
	c := config.Default()
	c.Backends[0].APIKey = "k"
	c.WebSearch.Via, c.Documents.Via = config.DefaultGatewayID, config.DefaultGatewayID
	RenameBackend(&c, config.DefaultGatewayID, "gw")
	if c.WebSearch.Via != "gw" || c.Documents.Via != "gw" {
		t.Errorf("%+v %+v", c.WebSearch, c.Documents)
	}
	c.Backends[0].APIKey = ""
	if p := strings.Join(Problems(&c), " "); !strings.Contains(p, "Web search goes through gw, which has no API key") {
		t.Error(p)
	}
	RemoveBackend(&c, "gw")
	if c.WebSearch.Via != "" || c.Documents.Via != "" {
		t.Error("removed")
	}
}

func TestExportImportRoundTripAsksForKeys(t *testing.T) {
	a := newTestApp(t)
	srv := gatewayStub(t, map[string]string{"main": "B"})
	src := fresh(t, srv)
	src.Backends[0].APIKey = "good"
	src.Backends = append(src.Backends, config.Backend{ID: "cn3", Kind: config.OpenAICompatible, BaseURL: "http://cn3/v1", APIKey: "sk-cn3"})
	EnableGateway(&src, config.DefaultGatewayID, a.Probe(context.Background(), src.Backends[0]), true)
	SetSearchChoice(&src, "off")
	if r := a.Save(src); !r.OK {
		t.Fatal(r)
	}
	path := filepath.Join(t.TempDir(), "team.codelink")
	if err := a.ExportTo(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "good") || strings.Contains(string(data), "sk-cn3") || strings.Contains(string(data), a.Config().AccessToken) {
		t.Fatalf("keys exported:\n%s", data)
	}
	p, err := PreviewImport(data)
	if err != nil || len(p.Providers) != 2 || len(p.Models) != 2 || p.Roles[0] != "Main: B" || p.Data != string(data) {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := PreviewImport([]byte(`{"hello":1}`)); err != share.ErrNotSettings {
		t.Errorf("%v", err)
	}

	// A colleague with a fresh code-link (a different app) imports it.
	b := newTestApp(t)
	draft := fresh(t, srv) // the same gateway address as the exporter's
	res, err := b.ImportInto(draft, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Needs) != 2 || res.Needs[0].ID != config.DefaultGatewayID || !res.Needs[0].Required || res.Needs[1].Required {
		t.Fatalf("needs %+v", res.Needs)
	}
	if res.View.Draft.Roles.Main != "B" || !res.View.Draft.WebSearch.Off || len(res.View.Draft.Backends) != 2 {
		t.Errorf("merged %+v", res.View.Draft)
	}
	// The built-in gateway was reused. Paste the key: tested, models
	// enabled, saved.
	k := b.ApplyKeys(context.Background(), res.View.Draft, map[string]string{config.DefaultGatewayID: "good", "cn3": "sk-mine"}, true)
	if !k.OK || !b.Configured() || b.Config().Roles.Main != "B" || b.Config().Backends[1].APIKey != "sk-mine" {
		t.Errorf("%+v", k)
	}
}
