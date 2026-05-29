// Package audit persists supply-chain policy decisions, ingest events,
// and admin actions to the audit_log table.
//
// Writes never block the request critical path: callers hand events to
// Logger.Log which enqueues them on a bounded buffered channel; a single
// drainer goroutine writes them serially to SQLite. On overflow the
// event is dropped and a counter incremented so operators can size the
// buffer.
//
// A retention pruner runs daily and deletes rows older than the owning
// tenant's audit_retention_days (default 90), defaulting to 90 days for
// tenant-less system events.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Event is a row of the audit log. Callers populate the structural
// fields; the writer fills in CreatedUnix if zero.
type Event struct {
	CreatedUnix int64

	UserID     int64
	TokenID    int64
	RequestID  string
	RemoteAddr string
	UserAgent  string

	// ActorKind distinguishes how the actor authenticated:
	//   "token"   PAT-backed (registry, admin REST)
	//   "session" web console password mode
	//   "proxy"   web console proxy-header mode
	//   ""        unknown / system event
	// Persisted to audit_log.actor_kind (schema v4).
	ActorKind string

	TenantID int64
	Action   string // "ingest" | "read" | "rule_create" | "rule_update" | "rule_delete" | "promote_quarantined" | …
	Format   string
	Package  string
	Version  string
	Filename string

	Decision string // "allow" | "warn" | "quarantine" | "deny" | "" (for non-policy events)
	RuleID   int64
	Reason   string

	Extra map[string]any // serialized to extra_json
}

// Logger is the public surface. It never blocks; overflow is observable
// via Drops().
type Logger interface {
	Log(Event)
	Drops() uint64
	Close() error
}

// New constructs a Logger backed by the audit_log table. bufSize is the
// channel buffer; default 4096 if zero.
func New(db *sql.DB, bufSize int) Logger {
	if bufSize <= 0 {
		bufSize = 4096
	}
	bl := &bufferedLogger{
		in:   make(chan Event, bufSize),
		done: make(chan struct{}),
		sink: dbSink{db: db},
	}
	bl.wg.Add(1)
	go bl.drain()
	return bl
}

type bufferedLogger struct {
	in    chan Event
	done  chan struct{}
	sink  sink
	wg    sync.WaitGroup
	drops uint64
	once  sync.Once
}

func (l *bufferedLogger) Log(e Event) {
	if e.CreatedUnix == 0 {
		e.CreatedUnix = time.Now().Unix()
	}
	select {
	case l.in <- e:
	default:
		atomic.AddUint64(&l.drops, 1)
	}
}

func (l *bufferedLogger) Drops() uint64 { return atomic.LoadUint64(&l.drops) }

func (l *bufferedLogger) Close() error {
	l.once.Do(func() {
		close(l.in)
	})
	l.wg.Wait()
	return nil
}

func (l *bufferedLogger) drain() {
	defer l.wg.Done()
	for ev := range l.in {
		if err := l.sink.Write(ev); err != nil {
			// Don't kill the drainer on a single failed write.
			log.Printf("audit: write failed: %v", err)
		}
	}
}

// sink abstracts the storage backend for testability.
type sink interface {
	Write(Event) error
}

type dbSink struct{ db *sql.DB }

func (s dbSink) Write(e Event) error {
	extra := "{}"
	if len(e.Extra) > 0 {
		b, err := json.Marshal(e.Extra)
		if err == nil {
			extra = string(b)
		}
	}
	var (
		userID    any = e.UserID
		tokenID   any = e.TokenID
		tenantID  any = e.TenantID
		ruleID    any = e.RuleID
		decision  any = e.Decision
		fmtCol    any = e.Format
		pkg       any = e.Package
		ver       any = e.Version
		filename  any = e.Filename
		requestID any = e.RequestID
		remote    any = e.RemoteAddr
		ua        any = e.UserAgent
		reason    any = e.Reason
		actorKind any = e.ActorKind
	)
	if e.UserID == 0 {
		userID = nil
	}
	if e.TokenID == 0 {
		tokenID = nil
	}
	if e.TenantID == 0 {
		tenantID = nil
	}
	if e.RuleID == 0 {
		ruleID = nil
	}
	if e.Decision == "" {
		decision = nil
	}
	if e.Format == "" {
		fmtCol = nil
	}
	if e.Package == "" {
		pkg = nil
	}
	if e.Version == "" {
		ver = nil
	}
	if e.Filename == "" {
		filename = nil
	}
	if e.RequestID == "" {
		requestID = nil
	}
	if e.RemoteAddr == "" {
		remote = nil
	}
	if e.UserAgent == "" {
		ua = nil
	}
	if e.Reason == "" {
		reason = nil
	}
	if e.ActorKind == "" {
		actorKind = nil
	}

	_, err := s.db.Exec(
		`INSERT INTO audit_log
		   (created_unix, actor_user_id, actor_token_id, actor_kind, request_id,
		    remote_addr, user_agent, tenant_id, action,
		    format, package, version, filename,
		    decision, rule_id, reason, extra_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.CreatedUnix, userID, tokenID, actorKind, requestID,
		remote, ua, tenantID, e.Action,
		fmtCol, pkg, ver, filename,
		decision, ruleID, reason, extra)
	if err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return nil
}

// StartPruner launches a goroutine that runs the retention pruner once
// at startup and then every 24 hours. It returns when ctx is cancelled.
func StartPruner(ctx context.Context, db *sql.DB) {
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		prune(db)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				prune(db)
			}
		}
	}()
}

func prune(db *sql.DB) {
	// Delete rows where the owning tenant's retention has elapsed.
	// Rows with NULL tenant_id (system events) use a 90-day default.
	now := time.Now().Unix()
	_, err := db.Exec(
		`DELETE FROM audit_log
		   WHERE (
		       tenant_id IS NOT NULL AND
		       created_unix < ? - COALESCE((SELECT audit_retention_days FROM tenants WHERE id = audit_log.tenant_id), 90) * 86400
		   ) OR (
		       tenant_id IS NULL AND created_unix < ? - 90 * 86400
		   )`,
		now, now)
	if err != nil {
		log.Printf("audit: prune failed: %v", err)
	}
}
