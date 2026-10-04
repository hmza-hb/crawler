package observe

import (
	"sync"
)

var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type Registry struct {
	mu sync.Mutex
}

func New() *Registry {
	return &Registry{}
}

type Counter struct {
	mu sync.Mutex
}

func (c *Counter) Inc(labelValues ...string) {}
func (c *Counter) Add(val float64, labelValues ...string) {}

type Gauge struct {
	mu sync.Mutex
}

func (g *Gauge) Set(val float64, labelValues ...string) {}

type Histogram struct {
	mu sync.Mutex
}

func (h *Histogram) Observe(val float64, labelValues ...string) {}

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{}
}

func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{}
}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	return &Histogram{}
}
