// Package engine is the local proxy: Anthropic Messages, OpenAI Responses
// and Chat Completions in, the configured backends out (docs/DESIGN.md
// §2–§5). It listens on 127.0.0.1 only and requires the access token.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"codelink/internal/chat"
	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/httpx"
	"codelink/internal/messages"
	"codelink/internal/responses"
	"codelink/internal/upstream"
)

// State is the engine's lifecycle state.
type State struct {
	Running bool   `json:"running"`
	Port    int    `json:"port,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Engine runs the server for the current configuration.
type Engine struct {
	token   string
	log     hooks.EventLog
	factory hooks.Factory
	tuning  core.Tuning
	client  *upstream.Client
	facts   *core.Facts
	env     atomic.Pointer[core.Env]

	mu         sync.Mutex
	server     *http.Server
	serverPort int // the configured port the server was started for
	state      State
}

// New builds an engine; nothing listens until Start. factory builds the
// feature hooks (nil = none).
func New(cfg config.Config, log hooks.EventLog, factory hooks.Factory) *Engine {
	return NewWithTuning(cfg, log, factory, core.DefaultTuning())
}

func NewWithTuning(cfg config.Config, log hooks.EventLog, factory hooks.Factory, tuning core.Tuning) *Engine {
	if log == nil {
		log = hooks.NopLog{}
	}
	client := upstream.NewClient()
	e := &Engine{token: cfg.AccessToken, log: log, factory: factory, tuning: tuning, client: client, facts: core.NewFacts(client)}
	e.env.Store(e.newEnv(cfg))
	return e
}

func (e *Engine) newEnv(cfg config.Config) *core.Env {
	c := cfg
	return core.NewEnv(&c, e.client, e.log, e.tuning, e.facts, e.factory)
}

// Env is the current environment.
func (e *Engine) Env() *core.Env { return e.env.Load() }

// State is the current state.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// Start listens on 127.0.0.1:port (0 = any free port).
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.server != nil {
		return nil
	}
	port := e.env.Load().Config.Port
	srv, bound, err := e.listen(port)
	if err != nil {
		e.state = State{Error: err.Error()}
		e.log.Record(hooks.Event{Kind: "error", Message: "engine: " + err.Error()})
		return err
	}
	e.server, e.serverPort, e.state = srv, port, State{Running: true, Port: bound}
	return nil
}

func (e *Engine) listen(port int) (*http.Server, int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, 0, fmt.Errorf("could not listen on 127.0.0.1:%d: %w", port, err)
	}
	srv := &http.Server{Handler: e.routes(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.log.Record(hooks.Event{Kind: "error", Message: "engine: " + err.Error()})
		}
	}()
	return srv, ln.Addr().(*net.TCPAddr).Port, nil
}

// Stop stops the server, letting requests in flight finish for a few seconds.
func (e *Engine) Stop() {
	e.mu.Lock()
	srv := e.server
	e.server, e.state = nil, State{}
	e.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			srv.Close()
		}
	}
}

// Apply switches to a new configuration. Routing, models, roles and hooks
// change for the next request; requests in flight finish on the one they
// started with. A new port starts a new listener and drains the old one.
func (e *Engine) Apply(cfg config.Config) error {
	e.env.Store(e.newEnv(cfg))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.server == nil || e.serverPort == cfg.Port {
		return nil
	}
	srv, bound, err := e.listen(cfg.Port)
	if err != nil {
		e.log.Record(hooks.Event{Kind: "error", Message: "engine: could not move to port " + strconv.Itoa(cfg.Port) + ": " + err.Error()})
		return err
	}
	old := e.server
	e.server, e.serverPort, e.state = srv, cfg.Port, State{Running: true, Port: bound}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		old.Shutdown(ctx)
	}()
	return nil
}

// Probe tests a backend: reachable, its models, context windows, logprobs
// and vision.
func (e *Engine) Probe(ctx context.Context, b config.Backend) core.Probe {
	return core.ProbeBackend(ctx, e.client, b)
}

type handler func(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte)

func (e *Engine) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]any{"status": "ok"}, nil)
	})
	v1 := http.NewServeMux()
	withBody := func(h handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			env := e.env.Load()
			body, ok := httpx.ReadBody(w, r, env.Tuning.MaxRequestBody)
			if !ok {
				return
			}
			h(env, w, r, body)
		}
	}
	// Anthropic clients (Claude Code) send anthropic-version and get the
	// Anthropic list; everyone else gets the OpenAI one.
	v1.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		modelsList(e.env.Load(), w, r.Header.Get("anthropic-version") != "")
	})
	v1.HandleFunc("POST /v1/messages", withBody(messages.Handle))
	v1.HandleFunc("POST /v1/messages/count_tokens", withBody(messages.CountTokens))
	v1.HandleFunc("POST /v1/responses", withBody(responses.Handle))
	v1.HandleFunc("POST /v1/chat/completions", withBody(chat.Handle))
	mux.Handle("/v1/", httpx.RequireToken(e.token, v1))
	return mux
}

// modelsList answers GET /v1/models: the configured models and the role
// aliases.
func modelsList(env *core.Env, w http.ResponseWriter, anthropic bool) {
	type entry struct{ id, display string }
	var entries []entry
	for _, m := range env.Config.Models {
		entries = append(entries, entry{m.ID, m.ID})
	}
	for _, a := range env.Routing.Aliases() {
		entries = append(entries, entry{a.Alias, a.Model.ID + " (" + a.Alias + ")"})
	}
	if !anthropic {
		data := []any{}
		for _, x := range entries {
			data = append(data, map[string]any{"id": x.id, "object": "model", "created": 1767225600, "owned_by": "code-link"})
		}
		httpx.WriteJSON(w, 200, map[string]any{"object": "list", "data": data}, nil)
		return
	}
	data := []any{}
	for _, x := range entries {
		data = append(data, map[string]any{"type": "model", "id": x.id, "display_name": x.display, "created_at": "2026-01-01T00:00:00Z"})
	}
	var first, last any
	if len(entries) > 0 {
		first, last = entries[0].id, entries[len(entries)-1].id
	}
	httpx.WriteJSON(w, 200, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last}, nil)
}
