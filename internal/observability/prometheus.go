package observability

import (
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Prometheus TEXT exposition format (version 0.0.4), hand-rolled so the
// project needs no prometheus/client_golang dependency (Task 2-h).
//
// RenderPrometheusText turns the Metrics snapshot (counters, latest-value
// latencies, bucketed histogram summaries) into the classic text format:
//
//      # HELP agentos_runs_total Total number of agent runs recorded.
//      # TYPE agentos_runs_total counter
//      agentos_runs_total 42
//
// Metric keys written by this package embed their labels in the key
// (httpMetricKey: name{label="value",...}); those labels are parsed back out
// and re-rendered properly escaped. Family names are emitted in sorted order
// for deterministic output.
// ---------------------------------------------------------------------------

// PrometheusFormat is the Content-Type of the text exposition endpoint.
const PrometheusFormat = "text/plain; version=0.0.4"

// wellKnownHelp pins the HELP text for the metric families AgentOS itself
// defines. Families without an entry get a generic HELP line so every family
// still carries the required "# HELP" header.
var wellKnownHelp = map[string]string{
	MetricRunsTotal:       "Total number of agent runs recorded.",
	MetricToolsTotal:      "Total number of tool executions recorded.",
	MetricRequestDuration: "Duration of HTTP requests in seconds.",
	MetricQueueLength:     "Number of tasks currently waiting in the run queue.",
	HTTPRequestsMetric:    "HTTP requests processed, by route, method and status.",
	HTTPDurationMetric:    "Most recent HTTP request duration in seconds (last-value gauge).",
}

// metricSample is one rendered sample line (without the family header).
type metricSample struct {
	suffix string // rendered after the family name (_bucket/_sum/_count)
	labels string // rendered `{a="b",c="d"}` or ""
	value  string
}

// RenderPrometheusText renders the full exposition body. queueLength controls
// the agentos_queue_length gauge: a negative value omits the family (no queue
// wired), any other value is rendered verbatim.
func RenderPrometheusText(counts map[string]int64, latency map[string]float64, summaries map[string]HistogramSummary, queueLength int) string {
	families := map[string][]metricSample{}
	types := map[string]string{}

	// Histograms first: base name -> _bucket/_sum/_count samples. Sample
	// order is construction order (buckets ascending by bound, then _sum,
	// then _count).
	for key, summary := range summaries {
		name, labels := parseMetricKey(key)
		family := sanitizeMetricName(name)
		if _, seen := types[family]; seen {
			continue
		}
		types[family] = "histogram"
		samples := make([]metricSample, 0, len(summary.Buckets)+2)
		for _, bound := range sortedBucketBounds(summary.Buckets) {
			if bound == formatInfBucket {
				continue // rendered explicitly below
			}
			samples = append(samples, metricSample{
				suffix: "_bucket",
				labels: joinLabels(labels, "le", bound),
				value:  strconv.FormatUint(summary.Buckets[bound], 10),
			})
		}
		samples = append(samples,
			metricSample{
				suffix: "_bucket",
				labels: joinLabels(labels, "le", formatInfBucket),
				value:  strconv.FormatUint(summary.Count, 10),
			},
			metricSample{
				suffix: "_sum",
				labels: renderLabels(labels),
				value:  formatPromFloat(summary.Sum),
			},
			metricSample{
				suffix: "_count",
				labels: renderLabels(labels),
				value:  strconv.FormatUint(summary.Count, 10),
			})
		families[family] = samples
	}

	// Counters.
	for key, value := range counts {
		name, labels := parseMetricKey(key)
		family := sanitizeMetricName(name)
		if _, seen := types[family]; seen {
			continue // a histogram family with the same name wins
		}
		types[family] = "counter"
		families[family] = append(families[family], metricSample{
			labels: renderLabels(labels),
			value:  strconv.FormatInt(value, 10),
		})
	}

	// Latest-value latencies as gauges; skip keys whose family was already
	// typed so a name is never rendered with two different types.
	for key, value := range latency {
		name, labels := parseMetricKey(key)
		family := sanitizeMetricName(name)
		if _, seen := types[family]; seen {
			continue
		}
		types[family] = "gauge"
		families[family] = append(families[family], metricSample{
			labels: renderLabels(labels),
			value:  formatPromFloat(value),
		})
	}

	// Queue depth gauge.
	if queueLength >= 0 {
		types[MetricQueueLength] = "gauge"
		families[MetricQueueLength] = append(families[MetricQueueLength], metricSample{
			value: strconv.Itoa(queueLength),
		})
	}

	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		help := wellKnownHelp[name]
		if help == "" {
			help = "AgentOS metric " + name
		}
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(escapeHelp(help))
		b.WriteString("\n# TYPE ")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(types[name])
		b.WriteString("\n")
		samples := families[name]
		if types[name] != "histogram" {
			// Deterministic ordering for counter/gauge samples. Histogram
			// samples keep their construction order so bucket lines stay in
			// ascending numeric bound order (a lexical sort would put "10"
			// before "2.5").
			sort.SliceStable(samples, func(i, j int) bool {
				if samples[i].labels != samples[j].labels {
					return samples[i].labels < samples[j].labels
				}
				return samples[i].value < samples[j].value
			})
		}
		for _, sample := range samples {
			b.WriteString(name)
			b.WriteString(sample.suffix)
			b.WriteString(sample.labels)
			b.WriteString(" ")
			b.WriteString(sample.value)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// sortedBucketBounds returns the bucket keys of the summary in numeric order
// with the "+Inf" bucket last.
func sortedBucketBounds(buckets map[string]uint64) []string {
	out := make([]string, 0, len(buckets))
	for le := range buckets {
		out = append(out, le)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i] == formatInfBucket {
			return false
		}
		if out[j] == formatInfBucket {
			return true
		}
		fi, erri := strconv.ParseFloat(out[i], 64)
		fj, errj := strconv.ParseFloat(out[j], 64)
		if erri == nil && errj == nil {
			return fi < fj
		}
		return out[i] < out[j]
	})
	return out
}

// metricLabel is one parsed label of a metric key.
type metricLabel struct {
	name  string
	value string
}

// parseMetricKey splits a metric key of the form `name` or
// `name{label="value",...}` (the format produced by httpMetricKey) into the
// bare metric name and its labels. Malformed label sections are treated as
// absent so unparsable keys still render under a sanitized family name.
func parseMetricKey(key string) (string, []metricLabel) {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return key, nil
	}
	name := key[:open]
	closing := strings.LastIndexByte(key, '}')
	if closing < open {
		return name, nil
	}
	raw := key[open+1 : closing]
	var labels []metricLabel
	for _, part := range splitTopLevel(raw) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		labelName := sanitizeLabelName(strings.TrimSpace(part[:eq]))
		value := strings.TrimSpace(part[eq+1:])
		value = strings.TrimPrefix(value, "\"")
		value = strings.TrimSuffix(value, "\"")
		labels = append(labels, metricLabel{name: labelName, value: unescapeLabelValue(value)})
	}
	return name, labels
}

// splitTopLevel splits on commas that are not inside a quoted string.
func splitTopLevel(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
			current.WriteRune(r)
		case r == '\\' && inQuotes:
			escaped = true
			current.WriteRune(r)
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, current.String())
	return parts
}

// sanitizeMetricName enforces [a-zA-Z_:][a-zA-Z0-9_:]*.
func sanitizeMetricName(name string) string {
	var b strings.Builder
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "agentos_unknown"
	}
	return b.String()
}

// sanitizeLabelName enforces [a-zA-Z_][a-zA-Z0-9_]*.
func sanitizeLabelName(name string) string {
	var b strings.Builder
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// renderLabels renders parsed labels in original order.
func renderLabels(labels []metricLabel) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l.name+"=\""+escapeLabelValue(l.value)+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// joinLabels renders parsed labels plus one extra label (used for le).
func joinLabels(labels []metricLabel, extraName, extraValue string) string {
	parts := make([]string, 0, len(labels)+1)
	for _, l := range labels {
		parts = append(parts, l.name+"=\""+escapeLabelValue(l.value)+"\"")
	}
	parts = append(parts, extraName+"=\""+escapeLabelValue(extraValue)+"\"")
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabelValue escapes a label value per the text format rules.
func escapeLabelValue(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(value)
}

// unescapeLabelValue reverses escapeLabelValue for values parsed from keys.
func unescapeLabelValue(value string) string {
	var b strings.Builder
	escaped := false
	for _, r := range value {
		if escaped {
			switch r {
			case 'n':
				b.WriteRune('\n')
			case '\\':
				b.WriteRune('\\')
			case '"':
				b.WriteRune('"')
			default:
				b.WriteRune('\\')
				b.WriteRune(r)
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	return b.String()
}

// escapeHelp escapes a HELP docstring (backslash and newline only).
func escapeHelp(help string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n").Replace(help)
}

// formatPromFloat renders a float the way the text format expects: shortest
// round-trip form, with +Inf/-Inf/NaN spelled out by FormatFloat itself.
func formatPromFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
