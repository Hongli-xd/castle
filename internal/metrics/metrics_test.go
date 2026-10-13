package metrics

// Tests for the label handling that silently dropped every label value in
// v1.1.0. Three production call sites passed []string{"name"} — a label name
// with no value — and the registry rendered the series as if no labels had
// been declared at all, so the dimension the caller meant to record vanished
// without any indication.

import (
	"strings"
	"testing"
)

func TestCounterRecordsNameValuePairs(t *testing.T) {
	r := New()
	// The shape the fixed call sites use.
	r.Counter("http_requests_total", "requests", []string{"status", "200"}, 1)
	r.Counter("http_requests_total", "requests", []string{"status", "500"}, 4)

	out := r.Render()
	if !strings.Contains(out, `http_requests_total{status="200"} 1`) {
		t.Fatalf("expected the 200 series with its label value, got:\n%s", out)
	}
	if !strings.Contains(out, `http_requests_total{status="500"} 4`) {
		t.Fatalf("expected the 500 series with its label value, got:\n%s", out)
	}
	// Two statuses must be two series, not one collapsed number.
	if strings.Contains(out, `http_requests_total{} `) {
		t.Fatalf("labels were dropped again:\n%s", out)
	}
}

func TestNameWithoutValueIsVisibleNotSilent(t *testing.T) {
	r := New()
	// The exact v1.1.0 mistake: a name with no value.
	r.Counter("broken_total", "misdeclared", []string{"status"}, 3)

	out := r.Render()
	if !strings.Contains(out, `broken_total{status="<missing>"} 3`) {
		t.Fatalf("a malformed label set must render visibly, got:\n%s", out)
	}
	if !strings.Contains(out, MalformedMetricName) {
		t.Fatalf("expected a self-reporting counter %s, got:\n%s", MalformedMetricName, out)
	}
}

func TestWellFormedCallsDoNotTripTheMalformedCounter(t *testing.T) {
	r := New()
	r.Counter("fine_total", "ok", []string{"a", "1", "b", "2"}, 1)
	r.Gauge("fine_gauge", "ok", nil, 1)

	if _, ok := r.Snapshot()[MalformedMetricName+"{}"]; ok {
		t.Fatalf("a well-formed call was counted as malformed")
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := New()
	// A tenant ID or path is caller-supplied text; an unescaped quote or
	// newline would corrupt the scrape of the entire registry.
	r.Counter("x_total", "x", []string{"path", "/a\"b\nc\\d"}, 1)

	out := r.Render()
	if !strings.Contains(out, `path="/a\"b\nc\\d"`) {
		t.Fatalf("label value was not escaped for the exposition format:\n%s", out)
	}
	// A raw newline inside a label would have split the sample across lines.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "x_total") && strings.Count(line, "x_total") != 1 {
			t.Fatalf("sample line was split: %q", line)
		}
	}
}

func TestKeyCannotCollideAcrossDifferentLabelSplits(t *testing.T) {
	r := New()
	// v1.1.0 joined names and values raw, so these two distinct label sets
	// both produced the key `m{a,1,2}` and overwrote each other.
	r.Counter("m", "m", []string{"a", "1,2"}, 10)
	r.Counter("m", "m", []string{"a,1", "2"}, 20)

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("two distinct label sets collapsed into %d series: %v", len(snap), snap)
	}
}

func TestGaugeSetsRatherThanAccumulates(t *testing.T) {
	r := New()
	r.Gauge("pool_size", "warm containers", nil, 5)
	r.Gauge("pool_size", "warm containers", nil, 2)
	if got := r.Render(); !strings.Contains(got, "pool_size 2") {
		t.Fatalf("gauge accumulated instead of being set:\n%s", got)
	}
}

func TestHelpAndTypeEmittedOncePerMetric(t *testing.T) {
	r := New()
	r.Counter("http_requests_total", "requests", []string{"status", "200"}, 1)
	r.Counter("http_requests_total", "requests", []string{"status", "500"}, 1)

	out := r.Render()
	if n := strings.Count(out, "# HELP http_requests_total"); n != 1 {
		t.Fatalf("expected one HELP line, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "# TYPE http_requests_total"); n != 1 {
		t.Fatalf("expected one TYPE line, got %d:\n%s", n, out)
	}
}
