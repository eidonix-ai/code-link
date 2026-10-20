package features

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"codelink/internal/automode"
	"codelink/internal/config"
	"codelink/internal/documents"
	"codelink/internal/hooks"
	"codelink/internal/images"
)

type nopCaller struct{}

func (nopCaller) Chat(context.Context, config.Model, map[string]any) (hooks.ChatResult, error) {
	return hooks.ChatResult{Status: 200}, nil
}

func ptr(b bool) *bool { return &b }

var (
	glm  = config.Model{ID: "GLM", BackendID: "b", Upstream: "glm"}
	vl   = config.Model{ID: "VL", BackendID: "b", Upstream: "vl", Vision: ptr(true)}
	noLP = config.Model{ID: "Flare", BackendID: "b", Upstream: "f", Logprobs: ptr(false)}
)

func cfg(models []config.Model, roles config.Roles) *config.Config {
	c := config.Default()
	c.Models = models
	c.Roles = roles
	return &c
}

func decisionID(h hooks.Hooks) string {
	m, ok := h.AutoMode.DecisionModel()
	if !ok {
		return ""
	}
	return m.ID
}

func TestMinimalConfigGivesOffVariants(t *testing.T) {
	h := Factory(cfg([]config.Model{glm}, config.Roles{Main: "GLM"}), nopCaller{}, nil)
	if h.Images != nil {
		t.Fatal("images without a vision role")
	}
	if _, err := h.WebSearch.Search(context.Background(), "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal(err)
	}
	if decisionID(h) != "GLM" { // main, logprobs unknown
		t.Fatal(decisionID(h))
	}
	if _, ok := h.Documents.(*documents.Service); !ok {
		t.Fatalf("%T", h.Documents)
	}
	if _, ok := h.AutoMode.(*automode.Head); !ok {
		t.Fatalf("%T", h.AutoMode)
	}
}

func TestRolesPickDecisionAndVisionModels(t *testing.T) {
	h := Factory(cfg([]config.Model{glm, vl, noLP}, config.Roles{Main: "Flare", Decision: "GLM", Vision: "VL"}), nopCaller{}, hooks.NopLog{})
	if decisionID(h) != "GLM" {
		t.Fatal(decisionID(h))
	}
	if _, ok := h.Images.(*images.Describer); !ok {
		t.Fatalf("%T", h.Images)
	}
	// a vision role naming no configured model gives no describer
	if h := Factory(cfg([]config.Model{glm}, config.Roles{Main: "GLM", Vision: "missing"}), nopCaller{}, nil); h.Images != nil {
		t.Fatal("images for a missing model")
	}
}

func TestMainWithoutLogprobsIsNotTheDecisionModel(t *testing.T) {
	if id := decisionID(Factory(cfg([]config.Model{noLP}, config.Roles{Main: "Flare"}), nopCaller{}, nil)); id != "" {
		t.Fatal(id)
	}
	if id := decisionID(Factory(cfg([]config.Model{glm}, config.Roles{Main: "GLM", Decision: "missing"}), nopCaller{}, nil)); id != "" {
		t.Fatal(id)
	}
	// declared logprobs on main
	m := glm
	m.Logprobs = ptr(true)
	if id := decisionID(Factory(cfg([]config.Model{m}, config.Roles{Main: "GLM"}), nopCaller{}, nil)); id != "GLM" {
		t.Fatal(id)
	}
}

type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func record(t *testing.T, body string) *recorder {
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, req)
		r.mu.Unlock()
		io.WriteString(w, body)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) requests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.reqs...)
}

func TestSearchersUseTheirKeyFromTheSettings(t *testing.T) {
	tavily := record(t, `{"results":[{"title":"t","url":"https://x.test","content":"c"}]}`)
	c := cfg([]config.Model{glm}, config.Roles{Main: "GLM"})
	c.WebSearch = config.WebSearch{Provider: config.Tavily, APIKey: "secret-key"}
	h := build(c, nopCaller{}, nil, deps{tavilyURL: tavily.URL})
	hits, err := h.WebSearch.Search(context.Background(), "q", nil, nil)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	if got := tavily.requests()[0].Header.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatal(got)
	}

	// provider set but no key -> unavailable, nothing sent
	none := record(t, "{}")
	c.WebSearch = config.WebSearch{Provider: config.Tavily, APIKey: " "}
	h2 := build(c, nopCaller{}, nil, deps{tavilyURL: none.URL})
	if _, err := h2.WebSearch.Search(context.Background(), "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal(err)
	}
	if len(none.requests()) != 0 {
		t.Fatal("sent without a key")
	}

	// a key but no provider (search off) -> unavailable
	c.WebSearch = config.WebSearch{APIKey: "k"}
	if _, err := build(c, nopCaller{}, nil, deps{tavilyURL: none.URL}).WebSearch.Search(context.Background(), "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal(err)
	}

	brave := record(t, `{"web":{"results":[]}}`)
	c.WebSearch = config.WebSearch{Provider: config.Brave, APIKey: "bk"}
	if _, err := build(c, nopCaller{}, nil, deps{braveURL: brave.URL}).WebSearch.Search(context.Background(), "q", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := brave.requests()[0].Header.Get("X-Subscription-Token"); got != "bk" {
		t.Fatal(got)
	}
}

func TestDocumentsFollowTheSettings(t *testing.T) {
	c := cfg([]config.Model{glm}, config.Roles{Main: "GLM"})
	c.Documents = config.Documents{Local: config.LocalOff}
	out := Factory(c, nopCaller{}, nil).Documents.Read(context.Background(), hooks.DocumentInput{Data: []byte("hi"), MediaType: "text/plain"})
	if out.Error != "document reading is turned off" {
		t.Fatalf("%+v", out)
	}
}

func TestGatewayProviderServesSearchAndDocumentsWithItsKey(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/search":
			io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"The Go language"}]}`)
		case "/layout-parsing":
			if r.Method == http.MethodGet {
				w.WriteHeader(405)
				return
			}
			io.WriteString(w, `{"result":{"layoutParsingResults":[{"markdown":{"text":"PAGE ONE"}}]}}`)
		}
	}))
	defer gw.Close()
	c := cfg([]config.Model{glm}, config.Roles{Main: "GLM"})
	c.Backends = []config.Backend{{ID: "gw", Kind: config.OpenAICompatible, BaseURL: gw.URL + "/v1", APIKey: "user-key"}}
	c.WebSearch = config.WebSearch{Via: "gw", Provider: config.Brave, APIKey: "ignored"}
	c.Documents = config.Documents{Via: "gw", PaddleOCR: "http://ignored.invalid/layout-parsing", Local: config.LocalOff}
	h := build(c, nopCaller{}, nil, deps{client: gw.Client()})
	hits, err := h.WebSearch.Search(context.Background(), "go", nil, nil)
	if err != nil || len(hits) != 1 || hits[0].URL != "https://go.dev" || hits[0].Snippet != "The Go language" {
		t.Fatalf("search: %v %+v", err, hits)
	}
	out := h.Documents.Read(context.Background(), hooks.DocumentInput{Data: []byte("%PDF-1.4"), MediaType: "application/pdf", Title: "a.pdf"})
	if out.Via != "paddleocr" || out.Text == "" {
		t.Fatalf("documents: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["POST /v1/search"] != "Bearer user-key" || seen["POST /layout-parsing"] != "Bearer user-key" {
		t.Fatalf("auth: %v", seen)
	}
}

func TestGatewayWithoutAKeyIsUnavailable(t *testing.T) {
	c := cfg([]config.Model{glm}, config.Roles{Main: "GLM"})
	c.Backends = []config.Backend{{ID: "gw", BaseURL: "https://llm.example/v1"}}
	c.WebSearch = config.WebSearch{Via: "gw"}
	if _, err := Factory(c, nopCaller{}, nil).WebSearch.Search(context.Background(), "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal(err)
	}
}
