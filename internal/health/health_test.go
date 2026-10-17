package health

import (
	"context"
	"errors"
	"testing"
)

func TestCheckerReportsEachDependency(t *testing.T) {
	checker := NewChecker(
		ProbeFunc{Dependency: "postgresql", CheckFunc: func(context.Context) error { return nil }},
		ProbeFunc{Dependency: "sqs", CheckFunc: func(context.Context) error { return errors.New("unavailable") }},
	)

	statuses := checker.Check(context.Background())
	if statuses["postgresql"] != "ok" {
		t.Errorf("postgresql status = %q, want ok", statuses["postgresql"])
	}
	if statuses["sqs"] != "unavailable" {
		t.Errorf("sqs status = %q, want unavailable", statuses["sqs"])
	}
}

func TestCheckerRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checker := NewChecker(
		ProbeFunc{Dependency: "postgresql", CheckFunc: func(context.Context) error { return nil }},
		ProbeFunc{Dependency: "sqs", CheckFunc: func(context.Context) error { return nil }},
	)

	statuses := checker.Check(ctx)
	if statuses["postgresql"] != "unavailable" || statuses["sqs"] != "unavailable" {
		t.Fatalf("statuses = %#v, want all unavailable after cancellation", statuses)
	}
}
