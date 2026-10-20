package chat

import (
	"net/http"

	"codelink/internal/apierr"
	"codelink/internal/core"
	"codelink/internal/httpx"
)

// Handle serves the route. Placeholder until the port lands.
func Handle(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte) {
	httpx.WriteAnthropic(w, apierr.API(501, "not implemented yet"))
}
