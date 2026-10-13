// Package metrics is a tiny Prometheus-text registry, stdlib-only.
// It deliberately does not attempt full prometheus client parity: the
// daemon needs counters, gauges, and histograms-by-buckets, labeled.
//
// # Label contract
//
// A labelSet is a FLATTENED sequence of name/value PAIRS:
//
//	[]string{"status", "200"}                  // one label
//	[]string{"action", "sandbox.stop"}         // one label
//	[]string{"method", "GET", "status", "200"} // two labels
//
// Passing only a name — []string{"status"} — used to be accepted and then
// silently rendered as no labels at all, so every caller that made that
// mistake published an unlabelled series and lost the dimension it meant to
// record. Three production call sites did exactly that (see the v1.1.1 fix in
// httpapi/accessLog, service.recordCreateMetrics and service.transition).
// A malformed set is now rendered visibly and counted, so the mistake shows up
// in the exposition instead of vanishing.
package metrics

import (
	"fmt"
	"strings"
	"sync"
)

type labelSet []string // flattened k1,v1,k2,v2

// valid reports whether the set is a whole number of name/value pairs.
func (l labelSet) valid() bool { return len(l)%2 == 0 }

type series struct {
	name   string
	help   string
	typ    string // counter|gauge
	labels labelSet
	value  float64
}

// MalformedMetricName surfaces the programming error described in the package
// doc. It is itself unlabelled, so recording it can never recurse. Exported so
// tests and operators can assert that no call site is passing a label name
// without a value.
const MalformedMetricName = "sandboxd_metrics_malformed_calls_total"

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

// key identifies a series. It is built from the same canonical rendering the
// exposition uses, so two label sets that render identically cannot collide
// (the old raw join of names and values could: ["a","1,2"] and ["a,1","2"]
// both produced `a,1,2`).
func key(name string, labels labelSet) string {
	return name + "{" + renderLabels(labels) + "}"
}

// Counter declares (once) and increments a monotonic counter.
func (r *Registry) Counter(name, help string, labels labelSet, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteMalformedLocked(name, labels)
	s := r.declareLocked(name, help, labels, "counter")
	s.value += v
}

// Gauge sets a gauge value.
func (r *Registry) Gauge(name, help string, labels labelSet, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteMalformedLocked(name, labels)
	s := r.declareLocked(name, help, labels, "gauge")
	s.value = v
}

func (r *Registry) declareLocked(name, help string, labels labelSet, typ string) *series {
	k := key(name, labels)
	s, ok := r.series[k]
	if !ok {
		s = &series{name: name, help: help, typ: typ, labels: labels}
		r.series[k] = s
		r.order = append(r.order, k)
	}
	return s
}

// noteMalformedLocked records that a caller passed an odd-length label set.
// Callers hold r.mu.
func (r *Registry) noteMalformedLocked(name string, labels labelSet) {
	if labels.valid() || name == MalformedMetricName {
		return
	}
	s := r.declareLocked(MalformedMetricName,
		"metric calls that passed a label name without a value", nil, "counter")
	s.value++
}

// escapeLabelValue escapes a label value per the exposition format. Without
// this a value containing a quote or a newline — a tenant-supplied string, a
// path — would break the scrape for the whole registry.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// escapeHelp escapes a HELP line.
func escapeHelp(h string) string {
	if !strings.ContainsAny(h, "\\\n") {
		return h
	}
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(h)
}

// renderLabels produces the inner text of a label block, without braces.
func renderLabels(labels labelSet) string {
	if len(labels) == 0 {
		return ""
	}
	var sb strings.Builder
	write := func(name, value string) {
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(name)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(value))
		sb.WriteByte('"')
	}
	for i := 0; i+1 < len(labels); i += 2 {
		write(labels[i], labels[i+1])
	}
	if len(labels)%2 == 1 {
		// A trailing name with no value. Render it as visibly broken rather
		// than dropping it: silence is what let the original bug live.
		write(labels[len(labels)-1], "<missing>")
	}
	return sb.String()
}

func fmtLabels(labels labelSet, extra string) string {
	inner := renderLabels(labels)
	if extra != "" {
		if inner != "" {
			inner += ","
		}
		inner += extra
	}
	if inner == "" {
		return ""
	}
	return "{" + inner + "}"
}

// Render produces the Prometheus text exposition format.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seenHelp := map[string]bool{}
	var sb strings.Builder
	for _, k := range r.order {
		s := r.series[k]
		if !seenHelp[s.name] {
			seenHelp[s.name] = true
			fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", s.name, escapeHelp(s.help), s.name, s.typ)
		}
		fmt.Fprintf(&sb, "%s%s %v\n", s.name, fmtLabels(s.labels, ""), s.value)
	}
	return sb.String()
}

// Snapshot returns the recorded values keyed by their canonical series key, for
// tests that need to assert on what was recorded.
func (r *Registry) Snapshot() map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]float64{}
	for _, k := range r.order {
		out[k] = r.series[k].value
	}
	return out
}
