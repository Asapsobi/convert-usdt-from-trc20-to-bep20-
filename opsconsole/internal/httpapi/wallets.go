package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// poolWallet is a pool wallet with what it has earned and holds.
type poolWallet struct {
	opclient.PoolWallet
	Profit *opclient.ProfitWallet
}

type walletsData struct {
	basePageData
	Chain, ChainName string
	Settings         *opclient.PoolSettings
	Wallets          []poolWallet
	InUse, Free      int
}

func poolChain(r *http.Request) (string, string, bool) {
	switch chi.URLParam(r, "chain") {
	case "bsc":
		return "bsc", "BSC", true
	case "tron":
		return "tron", "TRON", true
	}
	return "", "", false
}

func (s *Server) getWallets(w http.ResponseWriter, r *http.Request) {
	chain, name, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data := walletsData{basePageData: s.page(r, "wallets", "Deposit wallets"), Chain: chain, ChainName: name}
	data.Subtitle = "The reusable wallets customers pay into"
	if s.Relayd == nil {
		data.Err = errRelaydMissing
		s.Templates.Render(w, "wallets", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	var settings opclient.PoolSettings
	var pool []opclient.PoolWallet
	var profit opclient.ProfitWallets
	var errSettings, errPool error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); settings, errSettings = s.Relayd.GetPoolSettings(ctx, chain) }()
	go func() { defer wg.Done(); pool, errPool = s.Relayd.ListPool(ctx, chain) }()
	go func() { defer wg.Done(); profit, _ = s.Relayd.GetProfitWallets(ctx) }()
	wg.Wait()
	var errs []string
	if errSettings != nil {
		errs = append(errs, errSettings.Error())
	} else {
		data.Settings = &settings
	}
	if errPool != nil {
		errs = append(errs, errPool.Error())
	}
	byAddr := map[string]*opclient.ProfitWallet{}
	for i := range profit.Wallets {
		byAddr[strings.ToLower(profit.Wallets[i].Address)] = &profit.Wallets[i]
	}
	for _, pw := range pool {
		data.Wallets = append(data.Wallets, poolWallet{PoolWallet: pw, Profit: byAddr[strings.ToLower(pw.Address)]})
		switch {
		case pw.Lease != nil:
			data.InUse++
		case pw.Available && pw.Status == "ACTIVE":
			data.Free++
		}
	}
	if len(errs) > 0 {
		data.Err = strings.Join(errs, "; ")
	}
	s.Templates.Render(w, "wallets", data)
}

func (s *Server) postWalletSettings(w http.ResponseWriter, r *http.Request) {
	chain, _, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	max, _ := strconv.Atoi(r.FormValue("max_wallets"))
	settings := opclient.PoolSettings{MaxWallets: max, CooldownAfterUse: strings.TrimSpace(r.FormValue("cooldown_after_use")),
		CooldownAfterExpiry: strings.TrimSpace(r.FormValue("cooldown_after_expiry"))}
	s.relaydAction(w, r, "/wallets/"+chain, "relayd.pool.settings", chain,
		map[string]any{"max_wallets": max, "cooldown_after_use": settings.CooldownAfterUse, "cooldown_after_expiry": settings.CooldownAfterExpiry},
		func() error { return s.Relayd.PutPoolSettings(r.Context(), chain, settings) })
}

func (s *Server) postWalletAdd(w http.ResponseWriter, r *http.Request) {
	chain, _, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.relaydAction(w, r, "/wallets/"+chain, "relayd.pool.provision", chain, nil,
		func() error { return s.Relayd.ProvisionWallet(r.Context(), chain) })
}

func (s *Server) postWalletStatus(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chain, _, ok := poolChain(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		address := chi.URLParam(r, "address")
		action := "relayd.pool.disable"
		if enable {
			action = "relayd.pool.enable"
		}
		s.relaydAction(w, r, "/wallets/"+chain, action, chain+":"+address, nil,
			func() error { return s.Relayd.SetPoolWallet(r.Context(), chain, address, enable) })
	}
}
