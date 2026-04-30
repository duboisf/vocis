package telemetry

import (
	"context"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// recordingExporter captures every batch handed to it so tests can
// assert "what arrived at the wire and when". Each call to
// ExportSpans is one batch — preserving batch boundaries is the
// whole point of the trace-grouped processor we're testing.
type recordingExporter struct {
	mu      sync.Mutex
	batches [][]sdktrace.ReadOnlySpan
	exports int
	shutdown int
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exports++
	e.batches = append(e.batches, append([]sdktrace.ReadOnlySpan(nil), spans...))
	return nil
}

func (e *recordingExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdown++
	return nil
}

func (e *recordingExporter) batchCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.batches)
}

// TestTraceBatchProcessor_BuffersChildrenUntilRootEnds is the core
// contract: a child whose parent (the root) is still open must NOT
// be exported yet. The whole point of this processor is to keep
// children from flushing before their root, which is what causes
// Jaeger's "parent span ID=... is not in the trace" warnings.
func TestTraceBatchProcessor_BuffersChildrenUntilRootEnds(t *testing.T) {
	t.Parallel()

	exp := &recordingExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(newTraceBatchProcessor(exp)))
	tr := tp.Tracer("test")

	rootCtx, root := tr.Start(context.Background(), "root")
	_, child := tr.Start(rootCtx, "child")

	child.End()
	if got := exp.batchCount(); got != 0 {
		t.Fatalf("after child.End() with root still open: exporter saw %d batches, want 0 (child must stay buffered)", got)
	}

	root.End()
	if got := exp.batchCount(); got != 1 {
		t.Fatalf("after root.End(): exporter saw %d batches, want 1 (full trace flushed together)", got)
	}
	if len(exp.batches[0]) != 2 {
		t.Fatalf("export batch had %d spans, want 2 (root + child in same batch)", len(exp.batches[0]))
	}
}

// TestTraceBatchProcessor_IndependentTracesDoNotInterfere ensures
// two concurrent dictations (or recall + dictation) don't block
// each other: ending trace A's root must flush A's spans even
// while trace B is still in flight. Memory & latency are per-trace.
func TestTraceBatchProcessor_IndependentTracesDoNotInterfere(t *testing.T) {
	t.Parallel()

	exp := &recordingExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(newTraceBatchProcessor(exp)))
	tr := tp.Tracer("test")

	rootACtx, rootA := tr.Start(context.Background(), "rootA")
	_, childA := tr.Start(rootACtx, "childA")
	rootBCtx, rootB := tr.Start(context.Background(), "rootB")
	_, childB := tr.Start(rootBCtx, "childB")

	childA.End()
	childB.End()
	rootA.End() // flush trace A only
	if got := exp.batchCount(); got != 1 {
		t.Fatalf("after rootA.End() with rootB still open: %d batches, want 1", got)
	}
	if len(exp.batches[0]) != 2 {
		t.Fatalf("trace A batch had %d spans, want 2", len(exp.batches[0]))
	}

	rootB.End()
	if got := exp.batchCount(); got != 2 {
		t.Fatalf("after rootB.End(): %d batches, want 2", got)
	}
	if len(exp.batches[1]) != 2 {
		t.Fatalf("trace B batch had %d spans, want 2", len(exp.batches[1]))
	}
}

// TestTraceBatchProcessor_ShutdownDrainsPending covers the crash /
// process-exit path: if the root never ends (the user kills vocis
// mid-dictation), Shutdown must flush whatever is buffered so we
// at least get the partial trace, not silent loss.
func TestTraceBatchProcessor_ShutdownDrainsPending(t *testing.T) {
	t.Parallel()

	exp := &recordingExporter{}
	proc := newTraceBatchProcessor(exp)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(proc))
	tr := tp.Tracer("test")

	rootCtx, _ := tr.Start(context.Background(), "root")
	_, child := tr.Start(rootCtx, "child")
	child.End()
	// Note: root.End() intentionally NOT called.

	if err := proc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := exp.batchCount(); got != 1 {
		t.Fatalf("after Shutdown without root.End(): %d batches, want 1 (partial trace must still flush)", got)
	}
	if len(exp.batches[0]) != 1 {
		t.Fatalf("Shutdown batch had %d spans, want 1 (just the orphaned child)", len(exp.batches[0]))
	}
	if exp.shutdown != 1 {
		t.Fatalf("exporter Shutdown invoked %d times, want 1", exp.shutdown)
	}
}

// TestTraceBatchProcessor_ForceFlushDrainsWithoutShutdown is the
// "I want my pending spans NOW" path — used by `defer
// shutdownTelemetry` siblings or an explicit flush before a
// long-running operation.
func TestTraceBatchProcessor_ForceFlushDrainsWithoutShutdown(t *testing.T) {
	t.Parallel()

	exp := &recordingExporter{}
	proc := newTraceBatchProcessor(exp)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(proc))
	tr := tp.Tracer("test")

	rootCtx, _ := tr.Start(context.Background(), "root")
	_, child := tr.Start(rootCtx, "child")
	child.End()

	if err := proc.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if got := exp.batchCount(); got != 1 {
		t.Fatalf("after ForceFlush: %d batches, want 1", got)
	}
	if exp.shutdown != 0 {
		t.Fatalf("exporter Shutdown invoked %d times during ForceFlush, want 0", exp.shutdown)
	}
}

// TestTraceBatchProcessor_RootWithNoChildrenStillExports covers the
// degenerate single-span trace case (e.g. recall capture spans).
// We must not require children for a root to flush.
func TestTraceBatchProcessor_RootWithNoChildrenStillExports(t *testing.T) {
	t.Parallel()

	exp := &recordingExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(newTraceBatchProcessor(exp)))
	tr := tp.Tracer("test")

	_, root := tr.Start(context.Background(), "lonesome")
	root.End()

	if got := exp.batchCount(); got != 1 {
		t.Fatalf("childless root: %d batches, want 1", got)
	}
	if len(exp.batches[0]) != 1 {
		t.Fatalf("childless root batch had %d spans, want 1", len(exp.batches[0]))
	}
}
