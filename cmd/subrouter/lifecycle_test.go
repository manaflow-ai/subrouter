package main

import (
	"bytes"
	"testing"
)

type lifecycleController struct {
	restarted bool
}

func (c *lifecycleController) start() error    { return nil }
func (c *lifecycleController) stop() error     { return nil }
func (c *lifecycleController) restart() error  { c.restarted = true; return nil }
func (c *lifecycleController) installed() bool { return true }
func (c *lifecycleController) remove() error   { return nil }
func (c *lifecycleController) describe() string {
	return "fake.daemon"
}

func TestRestartWaitsForDaemonHealthBeforeReportingSuccess(t *testing.T) {
	controller := &lifecycleController{}
	healthChecks := 0
	var out bytes.Buffer
	waitForReady := func() bool {
		if out.Len() != 0 {
			t.Fatal("success reported before readiness check")
		}
		healthChecks++
		return controller.restarted
	}

	if err := runServerLifecycleWith(controller, "restart", waitForReady, &out); err != nil {
		t.Fatal(err)
	}
	if healthChecks != 1 {
		t.Fatalf("health checks = %d, want 1", healthChecks)
	}
	if got := out.String(); got != "restarted fake.daemon\n" {
		t.Fatalf("output = %q", got)
	}
}
