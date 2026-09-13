// Package alert is relayd's own narrow path to paging a human -- today,
// a single real, working implementation (LogAlerter) that makes an
// UNRECOVERABLE leg loudly, structurally visible in the process log
// rather than indistinguishable from every other slog.Error call in this
// service. It is deliberately NOT a real PagerDuty/Opsgenie/Slack
// integration: no such vendor has been chosen (the same open item
// docs/03-build/model-f-relay-build-prompts.md's own "Open items" names
// for the UNRECOVERABLE case -- "a support-ticket runbook with the
// chosen vendor... is an operational procedure, not something R5's code
// alone can close"). This package is the seam a real integration plugs
// into later, the same posture s1/internal/kmssign.KMSClient and
// screening/internal/holds.RefundEntryBuilder already take toward their
// own real-vendor-shaped gaps: an interface with one honest, working,
// non-fake default, not a stub that errors.
package alert

import (
	"context"
	"log/slog"
)

// Severity is this package's own closed set -- small on purpose; a real
// paging integration can map these onto its own richer taxonomy.
type Severity string

const (
	// SeverityCritical means a human needs to look at this before the
	// affected customer notices on their own -- real money is stuck with
	// no automatic recovery path left.
	SeverityCritical Severity = "CRITICAL"
)

// Alert is one page-worthy event.
type Alert struct {
	Severity   Severity
	ExternalID string // the relay leg's own external_id
	Reason     string // short, stable machine-readable reason code
	Detail     string // human-readable detail, safe to show in a ticket/page
}

// Alerter fires a Alert to whatever is watching. Fire should not block
// indefinitely and should not itself panic -- a paging integration being
// down must never take relayd's own orchestrate loop down with it.
type Alerter interface {
	Fire(ctx context.Context, a Alert) error
}

// LogAlerter is the only Alerter this package ships: a structured,
// deliberately loud slog.Error line, distinguishable from an ordinary
// error log by its own "alert_severity" attribute -- grep for that (or a
// real log pipeline's own equivalent filter) until a real paging
// integration replaces this.
type LogAlerter struct{}

// Fire implements Alerter.
func (LogAlerter) Fire(_ context.Context, a Alert) error {
	slog.Error("ALERT: relayd needs human attention",
		"alert_severity", string(a.Severity),
		"external_id", a.ExternalID,
		"reason", a.Reason,
		"detail", a.Detail,
	)
	return nil
}
