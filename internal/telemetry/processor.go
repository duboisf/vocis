package telemetry

import (
	"context"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// traceBatchProcessor buffers spans by trace id and flushes the
// whole trace as a single export batch when its root span ends.
// This guarantees children arrive at the exporter alongside (never
// before) their parents — eliminating Jaeger's "parent span ID=N
// is not in the trace; skipping clock skew adjustment" warnings.
//
// Why this is needed: the SDK's BatchSpanProcessor exports spans
// every WithBatchTimeout regardless of the trace's overall state.
// vocis dictations have a long-lived root (`vocis.dictation`,
// ~10s) and many short children (kitty.*, recorder.start, etc.)
// that flush ~2s after they end. Children arrive at the Jaeger
// collector before the root, the collector's clock-skew adjuster
// runs without a parent in sight, and the warning is persisted on
// the child span. By the time the root finally arrives, the warning
// is already in storage.
//
// Trade-offs vs sdktrace.BatchSpanProcessor:
//   - No mid-trace partial export. Spans buffer in memory until
//     the root ends. For vocis (~20 spans per dictation, kB-scale
//     payload) this is negligible.
//   - Process exit MUST call Shutdown or ForceFlush to drain
//     in-flight traces, otherwise an interrupted dictation drops
//     all its spans. We already defer shutdownTelemetry in serve,
//     recall, and transcribe entrypoints.
//   - A trace with multiple root spans (we don't produce these
//     today) flushes once per root and could partially under-report
//     if root B ends before root A — Jaeger would see B's children
//     without B's parent context. Acceptable: the trace shape we
//     actually emit is single-root.
type traceBatchProcessor struct {
	mu       sync.Mutex
	pending  map[trace.TraceID][]sdktrace.ReadOnlySpan
	exporter sdktrace.SpanExporter
}

func newTraceBatchProcessor(exporter sdktrace.SpanExporter) *traceBatchProcessor {
	return &traceBatchProcessor{
		pending:  make(map[trace.TraceID][]sdktrace.ReadOnlySpan),
		exporter: exporter,
	}
}

// OnStart is a no-op — the processor only acts at span end.
func (p *traceBatchProcessor) OnStart(_ context.Context, _ sdktrace.ReadWriteSpan) {}

// OnEnd routes a finished span into the per-trace buffer. When the
// span is the trace's local root, it triggers an immediate flush
// of the whole trace. A "local root" is a span with no parent or
// with a remote parent (cross-process — vocis doesn't have any
// today, but the check is correct).
func (p *traceBatchProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	parent := span.Parent()
	isRoot := !parent.IsValid() || parent.IsRemote()

	p.mu.Lock()
	traceID := span.SpanContext().TraceID()
	p.pending[traceID] = append(p.pending[traceID], span)
	var toExport []sdktrace.ReadOnlySpan
	if isRoot {
		toExport = p.pending[traceID]
		delete(p.pending, traceID)
	}
	p.mu.Unlock()

	if toExport != nil {
		// Best-effort export; we've already removed the trace from
		// pending so a slow/failing exporter can't backpressure
		// new traces. The OTel exporter handles retry/backoff
		// internally.
		_ = p.exporter.ExportSpans(context.Background(), toExport)
	}
}

// Shutdown flushes every still-pending trace (roots that never
// ended) and then shuts down the underlying exporter. This is
// called by the SDK's TracerProvider.Shutdown which we defer in
// each command's entrypoint.
func (p *traceBatchProcessor) Shutdown(ctx context.Context) error {
	allSpans := p.drainPending()
	var firstErr error
	if len(allSpans) > 0 {
		if err := p.exporter.ExportSpans(ctx, allSpans); err != nil {
			firstErr = err
		}
	}
	if err := p.exporter.Shutdown(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ForceFlush exports pending spans without shutting down the
// exporter. Useful before a long blocking operation if you want
// the in-flight traces visible in Jaeger immediately.
func (p *traceBatchProcessor) ForceFlush(ctx context.Context) error {
	allSpans := p.drainPending()
	if len(allSpans) == 0 {
		return nil
	}
	return p.exporter.ExportSpans(ctx, allSpans)
}

func (p *traceBatchProcessor) drainPending() []sdktrace.ReadOnlySpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return nil
	}
	var allSpans []sdktrace.ReadOnlySpan
	for _, spans := range p.pending {
		allSpans = append(allSpans, spans...)
	}
	p.pending = make(map[trace.TraceID][]sdktrace.ReadOnlySpan)
	return allSpans
}
