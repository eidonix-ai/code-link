package share

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"codelink/internal/config"
)

func ip(n int) *int { return &n }

func sample() config.Config {
	c := config.Default()
	c.Port = 19001
	c.AccessToken = "secret-token"
	c.Backends[0].APIKey = "sk-gateway"
	c.Backends = append(c.Backends, config.Backend{ID: "cn3", Kind: config.OpenAICompatible, BaseURL: "http://10.0.0.3:8000/v1", APIKey: "sk-cn3"})
	c.Models = []config.Model{
		{ID: "GLM-5.3", BackendID: "llm.eidonix.com", Upstream: "GLM-5.3-Flash", ContextWindow: ip(1_048_576)},
		{ID: "Qwen", BackendID: "cn3", Upstream: "Qwen3.6"},
	}
	c.Roles = config.Roles{Main: "GLM-5.3", Opus: "Qwen", Decision: "GLM-5.3"}
	c.Documents = config.Documents{Via: "llm.eidonix.com", Local: config.LocalTextOnly}
	c.WebSearch = config.WebSearch{Provider: config.Tavily, APIKey: "tvly-secret"}
	c.Launch = config.Launch{Terminal: "iterm", PermissionMode: "plan", OpenAtLogin: true, RecentFolders: []string{"/Users/d/secret-project"}}
	return c
}

func TestExportNeverCarriesKeysTokenPortOrLaunchSettings(t *testing.T) {
	data := string(Marshal(Export(sample())))
	for _, secret := range []string{"sk-gateway", "sk-cn3", "secret-token", "tvly-secret", "19001", "secret-project", "iterm", "plan", "apiKey", "accessToken", "port", "launch"} {
		if strings.Contains(data, secret) {
			t.Errorf("export contains %q:\n%s", secret, data)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["format"] != Format || raw["version"] != float64(1) {
		t.Errorf("header %v %v", raw["format"], raw["version"])
	}
	for _, k := range []string{"providers", "models", "roles", "documents", "webSearch"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	f, err := Parse([]byte(data))
	if err != nil || len(f.Providers) != 2 || f.Providers[1] != (Provider{"cn3", config.OpenAICompatible, "http://10.0.0.3:8000/v1"}) ||
		f.Documents.Via != "llm.eidonix.com" || f.WebSearch.Provider != config.Tavily || f.Roles.Opus != "Qwen" {
		t.Errorf("round trip %+v %v", f, err)
	}
}

func TestParseRefusesOtherFiles(t *testing.T) {
	for _, bad := range []string{`nope`, `{"format":"something"}`, `{"format":"code-link-settings","version":9}`,
		`{"format":"code-link-settings","version":1,"providers":[{"id":"a","kind":"weird","baseURL":"http://a"}]}`,
		`{"format":"code-link-settings","version":1,"providers":[{"id":"a","baseURL":"ftp://a"}]}`,
		`{"format":"code-link-settings","version":1,"providers":[{"id":"a","baseURL":"http://a"},{"id":"a","baseURL":"http://b"}]}`,
		`{"format":"code-link-settings","version":1,"providers":[],"models":[{"id":"m","backend":"x","upstream":"u"}]}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	f, err := Parse([]byte(`{"format":"code-link-settings","version":1,"providers":[{"id":"a","baseURL":"http://a/v1"}]}`))
	if err != nil || f.Providers[0].Kind != config.OpenAICompatible {
		t.Errorf("default kind: %+v %v", f, err)
	}
}

func TestSummarize(t *testing.T) {
	p := Summarize(Export(sample()))
	if !reflect.DeepEqual(p.Providers, []string{"llm.eidonix.com (Go-LLM-Proxy)", "cn3 (OpenAI-compatible)"}) ||
		!reflect.DeepEqual(p.Models, []string{"GLM-5.3 on llm.eidonix.com", "Qwen on cn3"}) ||
		!reflect.DeepEqual(p.Roles, []string{"Main: GLM-5.3", "Opus: Qwen", "Decision: GLM-5.3"}) {
		t.Errorf("%+v", p)
	}
}

func TestMergeIntoAFreshConfigReusesTheBuiltInGateway(t *testing.T) {
	fresh := config.Default()
	fresh.AccessToken = "mine"
	c, res := Merge(fresh, Export(sample()))
	if len(c.Backends) != 2 || c.Backends[0].ID != config.DefaultGatewayID || c.Backends[1].ID != "cn3" {
		t.Fatalf("%+v", c.Backends)
	}
	if !reflect.DeepEqual(res.NeedKey, []string{config.DefaultGatewayID, "cn3"}) {
		t.Errorf("need key %v", res.NeedKey)
	}
	if c.Roles.Main != "GLM-5.3" || c.Roles.Opus != "Qwen" || c.Documents.Via != config.DefaultGatewayID ||
		c.Documents.Local != config.LocalTextOnly || c.WebSearch.Provider != config.Tavily || c.WebSearch.APIKey != "" {
		t.Errorf("%+v %+v %+v", c.Roles, c.Documents, c.WebSearch)
	}
	if c.AccessToken != "mine" || c.Port != config.DefaultPort || len(c.Models) != 2 || *c.Models[0].ContextWindow != 1_048_576 {
		t.Errorf("%+v", c)
	}
}

func TestMergeRenamesCollisionsConsistently(t *testing.T) {
	mine := config.Default()
	mine.Backends = append(mine.Backends, config.Backend{ID: "cn3", Kind: config.Anthropic, BaseURL: "https://other/v1", APIKey: "k"})
	mine.Models = []config.Model{{ID: "Qwen", BackendID: "cn3", Upstream: "claude-x"}}
	mine.Roles.Main = "Qwen"
	f := Export(sample())
	f.WebSearch = WebSearch{Via: "cn3"}
	c, res := Merge(mine, f)
	if res.Providers["cn3"] != "cn3 2" || res.Models["Qwen"] != "Qwen 2" {
		t.Fatalf("renames %+v", res)
	}
	q, ok := c.Model("Qwen 2")
	if !ok || q.BackendID != "cn3 2" || q.Upstream != "Qwen3.6" {
		t.Errorf("renamed model %+v", q)
	}
	if c.Roles.Opus != "Qwen 2" || c.Roles.Main != "GLM-5.3" || c.WebSearch.Via != "cn3 2" {
		t.Errorf("roles and via follow: %+v %+v", c.Roles, c.WebSearch)
	}
	if mine0, _ := c.Model("Qwen"); mine0.BackendID != "cn3" {
		t.Error("the existing model stays")
	}
	if !reflect.DeepEqual(res.NeedKey, []string{config.DefaultGatewayID, "cn3 2"}) {
		t.Errorf("need key %v", res.NeedKey)
	}
}

func TestMergeReusesAKeyedProviderAndItsModels(t *testing.T) {
	mine := sample()
	mine.Models[0].ID = "my-glm"
	mine.Roles = config.Roles{Main: "my-glm"}
	f := Export(sample())
	c, res := Merge(mine, f)
	if len(c.Backends) != 2 || len(c.Models) != 2 || len(res.NeedKey) != 0 {
		t.Fatalf("%+v %+v", c.Backends, res)
	}
	if c.Roles.Main != "my-glm" || c.Roles.Decision != "my-glm" || c.Backends[0].APIKey != "sk-gateway" || c.WebSearch.APIKey != "tvly-secret" {
		t.Errorf("%+v %+v", c.Roles, c.WebSearch)
	}
	// A file without a main model leaves the roles alone.
	f.Roles = config.Roles{}
	if c2, _ := Merge(mine, f); c2.Roles.Main != "my-glm" {
		t.Error("roles kept")
	}
}
