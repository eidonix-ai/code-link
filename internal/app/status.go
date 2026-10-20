package app

import (
	"strconv"

	"codelink/internal/config"
	"codelink/internal/engine"
)

// StatusKind is what the tray icon and the status line show.
type StatusKind string

const (
	NotConfigured      StatusKind = "notConfigured"
	Stopped            StatusKind = "stopped"
	Starting           StatusKind = "starting"
	Running            StatusKind = "running"
	BackendUnreachable StatusKind = "backendUnreachable"
	Failed             StatusKind = "failed"
)

// Status is the engine's state in words.
type Status struct {
	Kind StatusKind `json:"kind"`
	Line string     `json:"line"`
	// Port is the port sessions connect to (the running one, else the
	// configured one).
	Port int `json:"port"`
	// Level colours the status dot: ok, warn, error, idle.
	Level string `json:"level"`
}

// MakeStatus words the engine's state. busyPort, when set, is the
// configured port that was taken, so the engine runs on another.
func MakeStatus(c *config.Config, st engine.State, starting bool, reachable *bool, busyPort int) Status {
	main, ok := c.Model(c.Roles.Main)
	if !ok || !c.Configured() {
		return Status{Kind: NotConfigured, Line: "Not configured — open Settings", Port: c.Port, Level: "idle"}
	}
	switch {
	case st.Running:
		if reachable != nil && !*reachable {
			return Status{Kind: BackendUnreachable, Line: main.BackendID + " is unreachable · " + main.ID, Port: st.Port, Level: "error"}
		}
		line := "Running on 127.0.0.1:" + strconv.Itoa(st.Port) + " · " + main.ID
		if busyPort != 0 && busyPort != st.Port {
			line += " (port " + strconv.Itoa(busyPort) + " was busy)"
		}
		return Status{Kind: Running, Line: line, Port: st.Port, Level: "ok"}
	case starting:
		return Status{Kind: Starting, Line: "Starting…", Port: c.Port, Level: "warn"}
	case st.Error != "":
		return Status{Kind: Failed, Line: "Failed — " + st.Error, Port: c.Port, Level: "error"}
	}
	return Status{Kind: Stopped, Line: "Stopped · " + main.ID, Port: c.Port, Level: "idle"}
}
