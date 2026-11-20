package core

import (
	"context"
	"time"
)

// UsageSummaryData is a small view of the week, for the overview page.
type UsageSummaryData struct {
	HasData  bool        `json:"has_data"`
	Days     int         `json:"days"`
	Totals   UsageTotals `json:"totals"`
	Quota    *UsageQuota `json:"quota"`
	LastSync *time.Time  `json:"last_sync"`
}

// UsageSummary is this week's tokens, cache hit rate and quota for the
// person themself.
func (s *Service) UsageSummary(ctx context.Context, actor *Actor) (*UsageSummaryData, error) {
	q := UsageQuery{Days: 7}
	sc, loc, _, _, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	d := &UsageSummaryData{Days: 7}
	if d.HasData, err = s.usageHasData(ctx, sc); err != nil || !d.HasData {
		return d, err
	}
	b, err := s.usageTokens(ctx, sc, book, loc)
	if err != nil {
		return nil, err
	}
	d.Totals = b.total.totals()
	if d.Quota, err = s.usageQuota(ctx, sc, 7); err != nil {
		return nil, err
	}
	if d.Quota != nil {
		for i := range d.Quota.Windows {
			d.Quota.Windows[i].History = nil
		}
	}
	ms, err := s.usageMachines(ctx, sc)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.LastSyncAt != nil && (d.LastSync == nil || m.LastSyncAt.After(*d.LastSync)) {
			d.LastSync = m.LastSyncAt
		}
	}
	return d, nil
}
