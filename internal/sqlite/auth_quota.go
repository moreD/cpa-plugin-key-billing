package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func (d *DB) AuthQuota(authIndex, provider string) (billing.AuthQuotaSnapshot, bool, error) {
	var snapshot billing.AuthQuotaSnapshot
	var fetchedAt, nextFetchAt int64
	var availableCount int
	var creditsJSON string
	err := d.db.QueryRow(`SELECT fetched_at, next_fetch_at, available_count, credits_json
		FROM auth_quota_snapshots WHERE auth_index = ? AND provider = ?`, authIndex, provider).
		Scan(&fetchedAt, &nextFetchAt, &availableCount, &creditsJSON)
	if err == sql.ErrNoRows {
		return billing.AuthQuotaSnapshot{}, false, nil
	}
	if err != nil {
		return billing.AuthQuotaSnapshot{}, false, fmt.Errorf("Read auth quota snapshot: %w", err)
	}
	var expirations []string
	if err := json.Unmarshal([]byte(creditsJSON), &expirations); err != nil {
		return billing.AuthQuotaSnapshot{}, false, fmt.Errorf("Parse auth quota snapshot: %w", err)
	}
	snapshot = billing.AuthQuotaSnapshot{
		AuthIndex: authIndex, Provider: provider,
		FetchedAt: timeAt(fetchedAt), NextFetchAt: timeAt(nextFetchAt),
		AvailableCount: availableCount, AvailableCountKnown: availableCount >= 0,
		CreditExpirations: expirations,
	}
	return snapshot, true, nil
}

func (d *DB) SaveAuthQuota(snapshot billing.AuthQuotaSnapshot) error {
	creditsJSON, err := json.Marshal(snapshot.CreditExpirations)
	if err != nil {
		return fmt.Errorf("Encode auth quota snapshot: %w", err)
	}
	availableCount := -1
	if snapshot.AvailableCountKnown {
		availableCount = snapshot.AvailableCount
	}
	_, err = d.db.Exec(`INSERT INTO auth_quota_snapshots
		(auth_index, provider, fetched_at, next_fetch_at, available_count, credits_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_index, provider) DO UPDATE SET
		fetched_at = excluded.fetched_at,
		next_fetch_at = excluded.next_fetch_at,
		available_count = excluded.available_count,
		credits_json = excluded.credits_json`,
		snapshot.AuthIndex, snapshot.Provider, nanos(snapshot.FetchedAt), nanos(snapshot.NextFetchAt), availableCount, string(creditsJSON))
	if err != nil {
		return fmt.Errorf("Write auth quota snapshot: %w", err)
	}
	return nil
}
