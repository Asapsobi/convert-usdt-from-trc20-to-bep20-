package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/opclient"
)

// back returns to path with an outcome message.
func back(w http.ResponseWriter, r *http.Request, path, key, msg string) {
	http.Redirect(w, r, path+"?"+url.Values{key: {msg}}.Encode(), http.StatusFound)
}

// --- Screening ---

type holdRow struct {
	ID, OrderID int64
	ExternalID  string
	ReasonCode  string
	OpenedAt    time.Time
	Status      string
}

type screeningData struct {
	basePageData
	Status string
	Holds  []holdRow
}

func (s *Server) getScreening(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "OPEN"
	}
	data := screeningData{basePageData: s.page(r, "screening", "Screening"), Status: status}
	data.Subtitle = "Orders held for a compliance check before any money moves"
	holds, err := s.Screening.ListHolds(r.Context(), status)
	if err != nil {
		data.Err = err.Error()
	}
	for _, h := range holds {
		data.Holds = append(data.Holds, holdRow{ID: h.ID, OrderID: h.OrderID, ExternalID: h.ExternalID, ReasonCode: h.ReasonCode, OpenedAt: h.OpenedAt, Status: h.Status})
	}
	s.Templates.Render(w, "screening", data)
}

func (s *Server) postHold(release bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid hold id", http.StatusBadRequest)
			return
		}
		_ = r.ParseForm()
		note := r.FormValue("note")
		sess, _ := sessionFromContext(r.Context())
		action := "screening.hold.release"
		if !release {
			action = "screening.hold.reject"
		}
		_ = s.Audit.Write(sess.Username, action, "hold:"+strconv.FormatInt(id, 10), map[string]any{"note": note})
		if release {
			err = s.Screening.ReleaseHold(r.Context(), id, sess.DisplayName, note)
		} else {
			err = s.Screening.RejectHold(r.Context(), id, sess.DisplayName, note)
		}
		if err != nil {
			back(w, r, "/screening", "err", err.Error())
			return
		}
		s.expireNavCounts()
		if release {
			back(w, r, "/screening", "ok", "Released: the order continues.")
		} else {
			back(w, r, "/screening", "ok", "Rejected: the order is refunded.")
		}
	}
}

// --- Signing approvals ---

type approvalRow struct {
	ID           int64
	SlotID       int
	EstimatedUSD float64
	CreatedAt    time.Time
}

type approvalsData struct {
	basePageData
	Requests         []approvalRow
	HasApproverToken bool
}

func (s *Server) getApprovals(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFromContext(r.Context())
	data := approvalsData{basePageData: s.page(r, "approvals", "Signing approvals"), HasApproverToken: sess.S1ApproverToken != ""}
	data.Subtitle = "Signatures worth more than the approval limit wait here for a person"
	pending, err := s.S1.ListPendingApprovals(r.Context())
	if err != nil {
		data.Err = err.Error()
	}
	for _, p := range pending {
		data.Requests = append(data.Requests, approvalRow{ID: p.ID, SlotID: p.SlotID, EstimatedUSD: p.EstimatedUSD, CreatedAt: p.CreatedAt})
	}
	s.Templates.Render(w, "approvals", data)
}

func (s *Server) postApproval(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid signing request id", http.StatusBadRequest)
			return
		}
		sess, _ := sessionFromContext(r.Context())
		if sess.S1ApproverToken == "" {
			back(w, r, "/approvals", "err", "Log in again with your approver token to approve or reject.")
			return
		}
		action := "s1.approval.approve"
		if !approve {
			action = "s1.approval.reject"
		}
		_ = s.Audit.Write(sess.Username, action, "signing-request:"+strconv.FormatInt(id, 10), nil)
		if approve {
			_, err = s.S1.Approve(r.Context(), id, sess.S1ApproverToken)
		} else {
			_, err = s.S1.Reject(r.Context(), id, sess.S1ApproverToken)
		}
		if err != nil {
			back(w, r, "/approvals", "err", err.Error())
			return
		}
		s.expireNavCounts()
		back(w, r, "/approvals", "ok", "Recorded.")
	}
}

// --- System ---

type systemData struct {
	basePageData
	Services     []serviceHealth
	BSC          *opclient.WatcherInvariants
	TRON         *opclient.TronwatcherInvariants
	Cursor       *opclient.Cursor
	LedgerHalted bool
	HaltText     string
	Version      string
}

func (s *Server) getSystem(w http.ResponseWriter, r *http.Request) {
	data := systemData{basePageData: s.page(r, "system", "System")}
	data.Subtitle = "Services, blockchain watchers and emergency controls"
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data.Services, _ = s.collectHealth(ctx)
	if s.Watcher != nil {
		if inv, err := s.Watcher.GetInvariants(ctx); err == nil {
			data.BSC = &inv
		}
		if cur, err := s.Watcher.GetCursor(ctx); err == nil {
			data.Cursor = &cur
		}
	}
	if s.Tronwatcher != nil {
		if inv, err := s.Tronwatcher.GetInvariants(ctx); err == nil {
			data.TRON = &inv
		}
	}
	if s.Ledger != nil {
		if halt, err := s.Ledger.GetHaltState(ctx); err == nil {
			data.LedgerHalted, data.HaltText = halt.Halted, halt.Reason
		}
	}
	if s.BuildInfo != nil {
		version, commit := s.BuildInfo()
		data.Version = version + " (" + commit + ")"
	}
	s.Templates.Render(w, "system", data)
}

func (s *Server) postHalt(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reason := r.FormValue("reason")
		sess, _ := sessionFromContext(r.Context())
		if set && reason == "" {
			back(w, r, "/system", "err", "Say why you are halting the ledger.")
			return
		}
		action, mode, msg := "ledger.halt.clear", "clear", "Halt cleared: money moves again."
		if set {
			action, mode, msg = "ledger.halt.set", "set", "Ledger halted: no money moves until you clear it."
		}
		_ = s.Audit.Write(sess.Username, action, "ledger", map[string]any{"reason": reason})
		if err := s.Ledger.SetHalt(r.Context(), mode, reason, sess.DisplayName); err != nil {
			back(w, r, "/system", "err", err.Error())
			return
		}
		back(w, r, "/system", "ok", msg)
	}
}

func (s *Server) postWatcherCursor(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	lastScanned, err1 := strconv.ParseInt(r.FormValue("last_scanned"), 10, 64)
	lastCandidate, err2 := strconv.ParseInt(r.FormValue("last_candidate_scanned"), 10, 64)
	reason := r.FormValue("reason")
	if err1 != nil || err2 != nil || reason == "" {
		back(w, r, "/system", "err", "Both block numbers must be integers, and a reason is required.")
		return
	}
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "watcher.cursor.set", "watcher", map[string]any{
		"last_scanned": lastScanned, "last_candidate_scanned": lastCandidate, "reason": reason,
	})
	if _, err := s.Watcher.SetCursor(r.Context(), lastScanned, lastCandidate, reason); err != nil {
		back(w, r, "/system", "err", err.Error())
		return
	}
	back(w, r, "/system", "ok", "BSC watcher position updated.")
}

// --- Audit log ---

type auditData struct {
	basePageData
	Entries []auditlog.Entry
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	data := auditData{basePageData: s.page(r, "audit", "Audit log")}
	data.Subtitle = "Every change made from this panel, newest first"
	entries, err := auditlog.Tail(s.AuditPath, 500)
	if err != nil {
		data.Err = err.Error()
	}
	for i := len(entries) - 1; i >= 0; i-- {
		data.Entries = append(data.Entries, entries[i])
	}
	s.Templates.Render(w, "audit", data)
}
