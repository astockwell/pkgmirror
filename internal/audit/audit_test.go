package audit

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/policy"
)

func TestBufferedLogger_WritesAndCloses(t *testing.T) {
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	l := New(db, 64)
	for i := 0; i < 5; i++ {
		l.Log(Event{
			TenantID: 1,
			Action:   "ingest",
			Format:   "pypi",
			Package:  "foo",
			Version:  "1.0.0",
			Decision: "allow",
		})
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 5 {
		t.Fatalf("rows persisted: got %d want 5", n)
	}
}

func TestBufferedLogger_DropsOnOverflow(t *testing.T) {
	// Use a tiny buffer and a slow sink to force drops.
	slow := &slowSink{}
	bl := &bufferedLogger{
		in:   make(chan Event, 2),
		done: make(chan struct{}),
		sink: slow,
	}
	bl.wg.Add(1)
	go bl.drain()

	slow.lock.Lock()
	// Fill the channel plus a few extras while drainer is held.
	for i := 0; i < 10; i++ {
		bl.Log(Event{Action: "read"})
	}
	slow.lock.Unlock()

	if err := bl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if bl.Drops() == 0 {
		t.Fatalf("expected drops > 0; got %d", bl.Drops())
	}
}

type slowSink struct {
	lock sync.Mutex
	n    int
}

func (s *slowSink) Write(_ Event) error {
	s.lock.Lock()
	s.n++
	s.lock.Unlock()
	return nil
}

func TestWrapEngine_FilterPolicy(t *testing.T) {
	got := &recordingLogger{}
	q := &stubQuerier{enabled: false}
	engine := WrapEngine(allowDeny{}, got, q)

	ctx := context.Background()

	// Ingest: always logged.
	engine.Evaluate(ctx, policy.Subject{TenantID: 1}, policy.ActionIngest)
	// Allow read with audit_reads off: NOT logged.
	engine.Evaluate(ctx, policy.Subject{TenantID: 1, Version: "ok"}, policy.ActionRead)
	// Non-allow read: always logged.
	engine.Evaluate(ctx, policy.Subject{TenantID: 1, Version: "bad"}, policy.ActionRead)

	if len(got.events) != 2 {
		t.Fatalf("expected 2 events, got %d: %#v", len(got.events), got.events)
	}
	if got.events[0].Action != "ingest" || got.events[0].Decision != "allow" {
		t.Errorf("event 0 = %+v", got.events[0])
	}
	if got.events[1].Action != "read" || got.events[1].Decision != "deny" {
		t.Errorf("event 1 = %+v", got.events[1])
	}

	// Now enable per-tenant read auditing and verify allow reads land.
	got.events = nil
	q.enabled = true
	engine.Evaluate(ctx, policy.Subject{TenantID: 1, Version: "ok"}, policy.ActionRead)
	if len(got.events) != 1 || got.events[0].Decision != "allow" {
		t.Fatalf("with audit_reads on: %#v", got.events)
	}
}

func TestWrapEngine_PullsActorFromContext(t *testing.T) {
	got := &recordingLogger{}
	engine := WrapEngine(policy.NoopEngine{}, got, &stubQuerier{enabled: false})
	ctx := policy.WithActor(context.Background(), policy.Actor{
		UserID: 7, TokenID: 11, RequestID: "abc", RemoteAddr: "1.2.3.4", UserAgent: "go-test",
	})

	engine.Evaluate(ctx, policy.Subject{TenantID: 1, Package: "foo"}, policy.ActionIngest)
	if len(got.events) != 1 {
		t.Fatalf("len=%d", len(got.events))
	}
	ev := got.events[0]
	if ev.UserID != 7 || ev.TokenID != 11 || ev.RequestID != "abc" ||
		ev.RemoteAddr != "1.2.3.4" || ev.UserAgent != "go-test" {
		t.Errorf("actor not propagated: %+v", ev)
	}
}

// --- test doubles ---

type recordingLogger struct {
	lock   sync.Mutex
	events []Event
}

func (l *recordingLogger) Log(e Event) {
	l.lock.Lock()
	defer l.lock.Unlock()
	if e.CreatedUnix == 0 {
		e.CreatedUnix = time.Now().Unix()
	}
	l.events = append(l.events, e)
}
func (l *recordingLogger) Drops() uint64 { return 0 }
func (l *recordingLogger) Close() error  { return nil }

type stubQuerier struct{ enabled bool }

func (s *stubQuerier) AuditReadsEnabled(_ context.Context, _ int64) bool { return s.enabled }

// allowDeny is a tiny inner engine: Decision is Deny when Subject.Version
// is "bad", Allow otherwise. Lets us probe the filter logic without
// pulling a real evaluator.
type allowDeny struct{}

func (allowDeny) Evaluate(_ context.Context, s policy.Subject, _ policy.Action) policy.Result {
	if s.Version == "bad" {
		return policy.Result{Decision: policy.Deny, Reason: "fixture"}
	}
	return policy.Result{Decision: policy.Allow}
}
