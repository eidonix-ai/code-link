package core

import (
	"reflect"
	"testing"
)

func names(ms []ProbedModel) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Upstream)
	}
	return out
}

func TestOfferedModelsFollowTheGatewayElseHideNonChatModels(t *testing.T) {
	all := []ProbedModel{{Upstream: "GLM-5.3-Flash"}, {Upstream: "nomic-embed"}, {Upstream: "jina-reranker"}, {Upstream: "bge-reranker"},
		{Upstream: "whisper-large"}, {Upstream: "kokoro-tts"}, {Upstream: "paddle-ocr"}, {Upstream: "deepseek-4.1-flash-1017"}}
	if got := names(OfferedModels(all, nil)); !reflect.DeepEqual(got, []string{"GLM-5.3-Flash", "deepseek-4.1-flash-1017"}) {
		t.Errorf("no gateway: %v", got)
	}
	if got := names(OfferedModels(all, &Gateway{})); !reflect.DeepEqual(got, []string{"GLM-5.3-Flash", "deepseek-4.1-flash-1017"}) {
		t.Errorf("gateway without a list: %v", got)
	}
	g := &Gateway{Models: []string{"deepseek-4.1-flash-1017", "GLM-5.3-Flash", "not-listed-by-models"}}
	if got := names(OfferedModels(all, g)); !reflect.DeepEqual(got, []string{"deepseek-4.1-flash-1017", "GLM-5.3-Flash"}) {
		t.Errorf("gateway list, in its order: %v", got)
	}
	if got := OfferedModels(all, &Gateway{Models: []string{}}); len(got) != 0 {
		t.Errorf("an empty list offers nothing: %v", got)
	}
}
