package config

import "testing"

func TestDefaultIsTheBuiltInGatewayWaitingForAKey(t *testing.T) {
	c := Default()
	if len(c.Backends) != 1 || c.Backends[0].Kind != GoLLMProxy || c.Backends[0].BaseURL != GoLLMProxyURL {
		t.Fatalf("%+v", c.Backends)
	}
	if _, ok := c.Gateway(); ok {
		t.Fatal("a gateway without a key is not usable")
	}
}

func TestSearchAndDocumentsUseTheGatewayUnlessSetUpOtherwise(t *testing.T) {
	c := Default()
	c.Backends[0].APIKey = "k"
	if b, ok := c.SearchVia(); !ok || b.ID != DefaultGatewayID {
		t.Fatal("search should go through the gateway")
	}
	if b, ok := c.DocumentsVia(); !ok || b.ID != DefaultGatewayID {
		t.Fatal("documents should go through the gateway")
	}
	c.WebSearch.Provider, c.Documents.PaddleOCR = Tavily, "http://ocr.local/layout-parsing"
	if _, ok := c.SearchVia(); ok {
		t.Fatal("own search provider wins")
	}
	if _, ok := c.DocumentsVia(); ok {
		t.Fatal("own PaddleOCR wins")
	}
	c.Backends = append(c.Backends, Backend{ID: "other", Kind: GoLLMProxy, BaseURL: "https://gw.example/v1"})
	c.WebSearch.Via = "other"
	if b, ok := c.SearchVia(); !ok || b.ID != "other" {
		t.Fatal("explicit via wins")
	}
}

func TestGatewayURLUsesTheRoot(t *testing.T) {
	if u := GatewayURL(Backend{BaseURL: "https://llm.eidonix.com/v1/"}, GatewaySearchPath); u != "https://llm.eidonix.com/v1/search" {
		t.Fatal(u)
	}
	if u := GatewayURL(Backend{BaseURL: "nope"}, GatewaySearchPath); u != "" {
		t.Fatal(u)
	}
}

func TestOffAndLocalOnlyKeepTheGatewayOut(t *testing.T) {
	c := Default()
	c.Backends[0].APIKey = "k"
	c.WebSearch.Off, c.Documents.LocalOnly = true, true
	if _, ok := c.SearchVia(); ok {
		t.Fatal("web search is off")
	}
	if _, ok := c.DocumentsVia(); ok {
		t.Fatal("documents are read locally only")
	}
	c.WebSearch.Via, c.Documents.Via = DefaultGatewayID, DefaultGatewayID
	if _, ok := c.SearchVia(); ok {
		t.Fatal("off wins over via")
	}
	if _, ok := c.DocumentsVia(); ok {
		t.Fatal("local only wins over via")
	}
}
