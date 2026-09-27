package httpapi

// Pages for the earlier "Model D" design (energy broker, payout
// dispatcher). They appear only when OC_BROKER_*/OC_DISPATCHER_* are set;
// the running product (Model F) doesn't use these services.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

type reservationRow struct {
	ID, OrderID               int64
	ExternalID, TargetAddress string
	EnergyUnits               int64
	Status, Vendor            string
	Deadline                  time.Time
}

type brokerData struct {
	basePageData
	Status       string
	Reservations []reservationRow
}

func (s *Server) getBroker(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "FAILED"
	}
	data := brokerData{basePageData: s.page(r, "legacy-broker", "Energy broker"), Status: status}
	data.Subtitle = "Model D, not used by the running product"
	rows, err := s.Broker.ListReservations(r.Context(), []string{status}, 100)
	if err != nil {
		data.Err = err.Error()
	}
	for _, res := range rows {
		data.Reservations = append(data.Reservations, reservationRow{ID: res.ID, OrderID: res.OrderID, ExternalID: res.ExternalID,
			TargetAddress: res.TargetAddress, EnergyUnits: res.EnergyUnits, Status: res.Status, Vendor: deref(res.Vendor), Deadline: res.Deadline})
	}
	s.Templates.Render(w, "legacy_broker", data)
}

type brokerReconcileData struct {
	basePageData
	ID int64
}

func (s *Server) getBrokerReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid reservation id", http.StatusBadRequest)
		return
	}
	data := brokerReconcileData{basePageData: s.page(r, "legacy-broker", "Reconcile reservation"), ID: id}
	s.Templates.Render(w, "legacy_broker_reconcile", data)
}

func (s *Server) postBrokerReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid reservation id", http.StatusBadRequest)
		return
	}
	path := "/legacy/broker/" + strconv.FormatInt(id, 10) + "/reconcile"
	_ = r.ParseForm()
	orderID, err1 := strconv.ParseInt(r.FormValue("order_id"), 10, 64)
	energyUnits, err2 := strconv.ParseInt(r.FormValue("energy_units"), 10, 64)
	if err1 != nil || err2 != nil {
		back(w, r, path, "err", "order_id and energy_units must be integers")
		return
	}
	req := opclient.ReconcileRequest{
		OrderID: orderID, Provider: r.FormValue("provider"), DelegationID: r.FormValue("delegation_id"),
		TargetAddress: r.FormValue("target_address"), EnergyUnits: energyUnits, CostTRX: r.FormValue("cost_trx"),
	}
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "broker.reservation.reconcile", "reservation:"+strconv.FormatInt(id, 10), map[string]any{"request": req})
	if _, err := s.Broker.ReconcileReservation(r.Context(), id, req); err != nil {
		back(w, r, path, "err", "reconciling: "+err.Error())
		return
	}
	http.Redirect(w, r, "/legacy/broker?status=CONFIRMED&ok=Reconciled.", http.StatusFound)
}

type fallbackRow struct {
	ID          int64
	Reason      string
	TriggeredAt time.Time
	ResolvedAt  *time.Time
}

type brokerFallbackData struct {
	basePageData
	Events []fallbackRow
}

func (s *Server) getBrokerFallback(w http.ResponseWriter, r *http.Request) {
	data := brokerFallbackData{basePageData: s.page(r, "legacy-broker", "Manual fallback events")}
	events, err := s.Broker.ListFallbackEvents(r.Context(), nil)
	if err != nil {
		data.Err = err.Error()
	}
	for _, e := range events {
		data.Events = append(data.Events, fallbackRow{ID: e.ID, Reason: e.Reason, TriggeredAt: e.TriggeredAt, ResolvedAt: e.ResolvedAt})
	}
	s.Templates.Render(w, "legacy_broker_fallback", data)
}

func (s *Server) postBrokerFallbackResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid event id", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	resolution := r.FormValue("resolution")
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "broker.fallback.resolve", "fallback-event:"+strconv.FormatInt(id, 10), map[string]any{"resolution": resolution})
	if err := s.Broker.ResolveFallbackEvent(r.Context(), id, resolution, sess.DisplayName); err != nil {
		back(w, r, "/legacy/broker/fallback", "err", "resolving: "+err.Error())
		return
	}
	back(w, r, "/legacy/broker/fallback", "ok", "Resolved.")
}

type slotRow struct {
	ID          int
	TronAddress string
	Status      string
	Balance     string
	TxCount     int64
}

type dispatcherData struct {
	basePageData
	Slots []slotRow
}

func (s *Server) getDispatcher(w http.ResponseWriter, r *http.Request) {
	data := dispatcherData{basePageData: s.page(r, "legacy-dispatcher", "Payout dispatcher")}
	data.Subtitle = "Model D, not used by the running product"
	slots, err := s.Dispatcher.ListSlots(r.Context())
	if err != nil {
		data.Err = err.Error()
	}
	for _, sl := range slots {
		data.Slots = append(data.Slots, slotRow{ID: sl.ID, TronAddress: sl.TronAddress, Status: sl.Status, Balance: sl.Balance, TxCount: sl.TxCount})
	}
	s.Templates.Render(w, "legacy_dispatcher", data)
}

func (s *Server) postDispatcherSlotRetire(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	immediate := r.FormValue("immediate") == "true"
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "dispatcher.slot.retire", "slot:"+strconv.Itoa(id), map[string]any{"immediate": immediate})
	if err := s.Dispatcher.RetireSlot(r.Context(), id, immediate); err != nil {
		back(w, r, "/legacy/dispatcher", "err", err.Error())
		return
	}
	back(w, r, "/legacy/dispatcher", "ok", "Slot retired.")
}
