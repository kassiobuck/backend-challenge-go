package health

import (
	"context"
	"fmt"
)

type Probe interface {
	Name() string
	Check(context.Context) error
}

type Checker struct {
	probes []Probe
}

func NewChecker(probes ...Probe) *Checker {
	return &Checker{probes: append([]Probe(nil), probes...)}
}

func (c *Checker) Check(ctx context.Context) map[string]string {
	type result struct {
		name  string
		error error
	}

	results := make(chan result, len(c.probes))
	if ctx.Err() != nil {
		statuses := make(map[string]string, len(c.probes))
		for _, probe := range c.probes {
			statuses[probe.Name()] = "unavailable"
		}
		return statuses
	}
	for _, probe := range c.probes {
		go func(probe Probe) {
			results <- result{name: probe.Name(), error: probe.Check(ctx)}
		}(probe)
	}

	statuses := make(map[string]string, len(c.probes))
	for range c.probes {
		select {
		case result := <-results:
			status := "ok"
			if result.error != nil {
				status = "unavailable"
			}
			statuses[result.name] = status
		case <-ctx.Done():
			for _, probe := range c.probes {
				if _, checked := statuses[probe.Name()]; !checked {
					statuses[probe.Name()] = "unavailable"
				}
			}
			return statuses
		}
	}

	return statuses
}

type ProbeFunc struct {
	Dependency string
	CheckFunc  func(context.Context) error
}

func (p ProbeFunc) Name() string {
	return p.Dependency
}

func (p ProbeFunc) Check(ctx context.Context) error {
	if p.CheckFunc == nil {
		return fmt.Errorf("%s readiness check is not configured", p.Dependency)
	}
	return p.CheckFunc(ctx)
}
