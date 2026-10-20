// Package harness starts an engine against a stub backend for tests. Use it
// from external test packages (package messages_test, …) so the engine's
// imports don't cycle.
package harness

import (
	"testing"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/engine"
	"codelink/internal/hooks"
	"codelink/internal/testkit"
)

// H is a running engine with its stub backend.
type H struct {
	testkit.Client
	Engine *engine.Engine
	Stub   *testkit.Stub
	Log    *testkit.Log
}

// Options configure a run; zero values are fine.
type Options struct {
	Hooks     hooks.Hooks
	Factory   hooks.Factory // overrides Hooks
	Tuning    *core.Tuning  // nil = FastTuning
	Configure func(*config.Config)
}

// Run starts the stub (scripted by backend) and an engine on a free port,
// and stops both when the test ends.
func Run(t testing.TB, o Options, backend func(testkit.Seen, int) testkit.Reply) *H {
	t.Helper()
	stub := testkit.StartStub(t, backend)
	cfg := testkit.DefaultConfig(stub)
	if o.Configure != nil {
		o.Configure(&cfg)
	}
	tuning := testkit.FastTuning()
	if o.Tuning != nil {
		tuning = *o.Tuning
	}
	factory := o.Factory
	if factory == nil {
		factory = testkit.Factory(o.Hooks)
	}
	log := &testkit.Log{}
	e := engine.NewWithTuning(cfg, log, factory, tuning)
	if err := e.Start(); err != nil {
		t.Fatalf("engine start: %v", err)
	}
	t.Cleanup(e.Stop)
	return &H{Client: testkit.Client{Port: e.State().Port}, Engine: e, Stub: stub, Log: log}
}

// OK answers every chat request with "ok".
func OK(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, testkit.Text("ok")) }
