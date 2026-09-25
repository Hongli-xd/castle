// Package metrics is a tiny Prometheus-text registry, stdlib-only.
// It deliberately does not attempt full prometheus client parity: the
// daemon needs counters, gauges, and histograms-by-buckets, labeled.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type labelSet []string // flattened k1,v1,k2,v2

type series struct {
	help   string
	typ    string // counter|gauge
	labels labelSet
	value  float64
	extra  map[string]float64 // label-suffix → value (for per-label counters)
}

// Registry is safe for concurrent use; exposition format is rendered on
// demand for /metrics.
type Registry struct {
	mu     sync.Mutex
	series map[string]*series
	order  []string
}

func New() *Registry {
	return &Registry{series: map[string]*series{}}
}

func key(name string, labels labelSet) string {
	return name + "{" + strings.Join(labels, ",") + "}"
}

// Counter declares (once) and increments a monotonic counter.
func (r *Registry) Counter(name, help string, labels labelSet, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(name, labels)
	s, ok := r.series[k]
	if !ok {
		s = &series{help: help, typ: "counter", labels: labels}
		r.series[k] = s
		r.order = append(r.order, k)
	}
	s.value += v
}

// Gauge sets a gauge value.
func (r *Registry) Gauge(name, help string, labels labelSet, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(name, labels)
	s, ok := r.series[k]
	if !ok {
		s = &series{help: help, typ: "gauge", labels: labels}
		r.series[k] = s
		r.order = append(r.order, k)
	}
	s.value = v
}

func fmtLabels(labels labelSet, extra string) string {
	parts := make([]string, 0, len(labels)/2+1)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, labels[i]+`="`+labels[i+1]+`"`)
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Render produces the Prometheus text exposition format.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seenHelp := map[string]string{}
	var sb strings.Builder
	for _, k := range r.order {
		s := r.series[k]
		name := k[:strings.IndexByte(k, '{')]
		if _, ok := seenHelp[name]; !ok {
			seenHelp[name] = s.typ
			fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", name, s.help, name, s.typ)
		}
		fmt.Fprintf(&sb, "%s%s %v\n", name, fmtLabels(s.labels, ""), s.value)
	}
	return sb.String()
}

// SortedSnapshot is used by tests to assert on recorded values.
func (r *Registry) Snapshot() map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]float64{}
	for _, k := range r.order {
		out[k] = r.series[k].value
	}
	return out
}

var _ = sort.Strings
