package plugin

import (
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
	smartbalancer "github.com/nitansde/smart-load-balancer/balancer"
	smartquota "github.com/nitansde/smart-load-balancer/quota"
)

// resettableQuotaLedger keeps manual reset support in the plugin rather than
// changing the pinned scheduler dependency. Only the redeemed backoff is
// suppressed; usage remains intact and a new quota failure can block again.
type resettableQuotaLedger struct {
	mu            sync.Mutex
	ledger        *smartquota.Ledger
	clearedBlocks map[string]time.Time
}

func newResettableQuotaLedger() *resettableQuotaLedger {
	return &resettableQuotaLedger{ledger: smartquota.NewLedger(), clearedBlocks: make(map[string]time.Time)}
}

func (l *resettableQuotaLedger) Observe(observation smartquota.UsageObservation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ledger.Observe(observation)
	if _, cleared := l.clearedBlocks[observation.AuthID]; cleared && observation.Failed && observation.StatusCode == 429 {
		// Weekly failures can reuse the same reset deadline. Ask a fresh ledger
		// whether this observation creates a block, using the dependency's own
		// distinction between quota exhaustion and transient concurrency limits.
		probe := smartquota.NewLedger()
		probe.Observe(observation)
		if entry, _ := probe.Get(observation.AuthID); !entry.BlockedUntil.IsZero() {
			delete(l.clearedBlocks, observation.AuthID)
		}
	}
	if entry, found := l.ledger.Get(observation.AuthID); found && !entry.BlockedUntil.Equal(l.clearedBlocks[observation.AuthID]) {
		delete(l.clearedBlocks, observation.AuthID)
	}
}

func (l *resettableQuotaLedger) Get(authID string) (smartquota.LedgerEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.ledger.Get(authID)
	if cleared, ok := l.clearedBlocks[authID]; ok && entry.BlockedUntil.Equal(cleared) {
		entry.BlockedUntil = time.Time{}
		entry.BlockReason = ""
	}
	return entry, found
}

func (l *resettableQuotaLedger) ClearBlock(authID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, found := l.ledger.Get(authID); found && !entry.BlockedUntil.IsZero() {
		l.clearedBlocks[authID] = entry.BlockedUntil
	}
}

// billingQuotaResolver adapts the upstream scheduler's quota resolver to the
// billing plugin's persisted snapshots. The scheduler candidate ID is CPA's
// runtime auth ID; quota snapshots and usage events use CPA's stable auth
// index, so the mapping is maintained from host.auth.list.
type billingQuotaResolver struct {
	app *App
	now func() time.Time
}

func (r *billingQuotaResolver) Lookup(authID, provider string) smartbalancer.QuotaInfo {
	if r == nil || r.app == nil || r.app.store == nil {
		return smartbalancer.QuotaInfo{}
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	index := r.app.authIndexForCredential(authID)
	if index == "" {
		// Tests and older hosts may expose the stable index as the candidate ID.
		index = strings.TrimSpace(authID)
	}

	info := smartbalancer.QuotaInfo{}
	if entry, ok := r.app.smartLedger.Get(authID); ok {
		info.Known = true
		info.ConsumedTokens = entry.ConsumedTokens
		info.Fresh = entry.Fresh()
		if entry.Blocked(now) {
			info.BlockedUntil = entry.BlockedUntil
		}
	} else if index != authID {
		if entry, ok := r.app.smartLedger.Get(index); ok {
			info.Known = true
			info.ConsumedTokens = entry.ConsumedTokens
			info.Fresh = entry.Fresh()
			if entry.Blocked(now) {
				info.BlockedUntil = entry.BlockedUntil
			}
		}
	}

	provider = strings.ToLower(strings.TrimSpace(provider))
	snapshot, found, errSnapshot := r.app.store.AuthQuota(index, provider)
	if errSnapshot == nil && found {
		info.Known = true
		info.Fresh = false
		parsed := billingQuotaSnapshot(snapshot)
		if parsed.Long != nil {
			info.UsedPercent = parsed.Long.UsedPercent
			info.LongResetAt = parsed.Long.NextReset(now)
		}
		if longWindowResetPassed(parsed, now) {
			// The snapshot is stale after the reset. Borrow one suitable real
			// request so its response headers refresh the persisted snapshot.
			r.app.smartDivert.Pending.Mark(authID)
		}
		cfg := r.app.store.Config()
		if cfg.SmartFiveHourBoost && parsed.FiveHourFresh(now) {
			r.app.smartDivert.Pending.Mark(authID)
		}
		if parsed.Exhausted() {
			if reset := parsed.EarliestReset(); reset.After(now) &&
				(info.BlockedUntil.IsZero() || reset.Before(info.BlockedUntil)) {
				info.BlockedUntil = reset
			}
		}
	}
	if !info.Known && smartquota.HasEndpoint(provider) {
		// For a quota-tracked provider, an untouched profile is presumed full
		// and is marked for one calibration diversion on first use.
		info.Known = true
		info.Fresh = true
		r.app.smartDivert.Pending.Mark(authID)
	}
	return info
}

func (a *App) authIndexForCredential(authID string) string {
	a.routingMu.Lock()
	defer a.routingMu.Unlock()
	return strings.TrimSpace(a.authIndexByCredential[strings.TrimSpace(authID)])
}

func (a *App) credentialIDForAuthIndex(authIndex string) string {
	a.routingMu.Lock()
	defer a.routingMu.Unlock()
	return strings.TrimSpace(a.credentialIDByIndex[strings.TrimSpace(authIndex)])
}

func (a *App) schedulerAuthID(authIndex string) string {
	if id := a.credentialIDForAuthIndex(authIndex); id != "" {
		return id
	}
	return strings.TrimSpace(authIndex)
}

func billingQuotaSnapshot(snapshot billing.AuthQuotaSnapshot) smartquota.Snapshot {
	parsed := smartquota.Snapshot{
		AuthID: snapshot.AuthIndex, Provider: snapshot.Provider, FetchedAt: snapshot.FetchedAt,
	}
	for _, row := range snapshot.Quota {
		window := smartQuotaWindow(row)
		if window == nil {
			continue
		}
		switch window.Kind {
		case smartquota.WindowFiveHour:
			parsed.FiveHour = window
		case smartquota.WindowWeekly, smartquota.WindowMonthly:
			parsed.Long = window
		}
	}
	return parsed
}

func smartQuotaWindow(row billing.AuthQuotaRow) *smartquota.Window {
	label := strings.ToLower(strings.TrimSpace(row.Label + " " + row.GroupLabel + " " + row.LabelPrefix))
	kind := smartquota.WindowKind("")
	switch {
	case strings.Contains(label, "5-hour"), strings.Contains(label, "5 hour"), strings.Contains(label, "5h"), strings.Contains(label, "300-minute"):
		kind = smartquota.WindowFiveHour
	case strings.Contains(label, "weekly"), strings.Contains(label, "7-day"), strings.Contains(label, "7 day"):
		kind = smartquota.WindowWeekly
	case strings.Contains(label, "monthly"), strings.Contains(label, "30-day"), strings.Contains(label, "30 day"):
		kind = smartquota.WindowMonthly
	default:
		return nil
	}
	var used *float64
	if row.Used != nil {
		value := *row.Used
		used = &value
	} else if row.RemainingPercent != nil {
		value := 100 - *row.RemainingPercent
		used = &value
	}
	window := &smartquota.Window{Kind: kind, UsedPercent: used}
	if value, err := time.Parse(time.RFC3339, strings.TrimSpace(row.ResetAt)); err == nil {
		window.ResetAt = value
	}
	if used != nil && *used >= 100 {
		window.Exhausted = true
	}
	return window
}

func longWindowResetPassed(snapshot smartquota.Snapshot, now time.Time) bool {
	return snapshot.Long != nil && !snapshot.Long.ResetAt.IsZero() && !snapshot.Long.ResetAt.After(now)
}
