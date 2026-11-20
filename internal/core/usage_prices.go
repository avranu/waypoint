package core

import (
	"context"
	"slices"
	"strings"
	"time"
)

// What a model's tokens would cost on the API (WP-D20). Subscriptions are
// not billed per token, so this is "API-equivalent": a way to compare
// models and sessions, not an invoice. Prices are per million tokens,
// matched to a model id by prefix (the longest wins), and editable by
// admins. A price of 0 means unknown.

// ModelPrice is one row of the price table.
type ModelPrice struct {
	Model        string     `json:"model"` // an id prefix: claude-opus-5-5
	Input        float64    `json:"input"`
	Output       float64    `json:"output"`
	CacheWrite5m float64    `json:"cache_write_5m"`
	CacheWrite1h float64    `json:"cache_write_1h"`
	CacheRead    float64    `json:"cache_read"`
	UpdatedAt    *time.Time `json:"updated_at"` // nil for the seeded prices
}

// Cost is the API-equivalent cost of the tokens, in USD.
func (p ModelPrice) Cost(input, output, cw5, cw1, cr int64) float64 {
	return (float64(input)*p.Input + float64(output)*p.Output + float64(cw5)*p.CacheWrite5m +
		float64(cw1)*p.CacheWrite1h + float64(cr)*p.CacheRead) / 1e6
}

// priceBook finds the price of a model id.
type priceBook struct {
	list []ModelPrice // longest model first
}

func newPriceBook(list []ModelPrice) *priceBook {
	b := &priceBook{list: slices.Clone(list)}
	slices.SortFunc(b.list, func(x, y ModelPrice) int { return len(y.Model) - len(x.Model) })
	return b
}

// find returns the price for model, if there is a known one.
func (b *priceBook) find(model string) (ModelPrice, bool) {
	m := strings.ToLower(model)
	for _, p := range b.list {
		if p.Model != "" && strings.HasPrefix(m, strings.ToLower(p.Model)) {
			return p, p.Input > 0 || p.Output > 0
		}
	}
	return ModelPrice{}, false
}

func (s *Service) loadPrices(ctx context.Context) (*priceBook, []ModelPrice, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT model, input, output, cache_write_5m, cache_write_1h, cache_read, updated_at FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var list []ModelPrice
	for rows.Next() {
		var p ModelPrice
		var at int64
		if err := rows.Scan(&p.Model, &p.Input, &p.Output, &p.CacheWrite5m, &p.CacheWrite1h, &p.CacheRead, &at); err != nil {
			return nil, nil, err
		}
		if at > 0 {
			t := msTime(at)
			p.UpdatedAt = &t
		}
		list = append(list, p)
	}
	return newPriceBook(list), list, rows.Err()
}

// ListModelPrices returns the price table.
func (s *Service) ListModelPrices(ctx context.Context, actor *Actor) ([]ModelPrice, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	_, list, err := s.loadPrices(ctx)
	if list == nil {
		list = []ModelPrice{}
	}
	return list, err
}

// SetModelPrice adds or changes a model's prices. Admins only, and not agents.
func (s *Service) SetModelPrice(ctx context.Context, actor *Actor, p ModelPrice) (*ModelPrice, error) {
	if err := requireUsageAdmin(actor); err != nil {
		return nil, err
	}
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" || len(p.Model) > 100 || strings.ContainsAny(p.Model, " \t\n") {
		return nil, invalid("model is an id such as claude-opus-5-5")
	}
	for _, v := range []float64{p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead} {
		if v < 0 || v > 100000 {
			return nil, invalid("prices are USD per million tokens, from 0 up")
		}
	}
	now := s.nowMS()
	err := s.write(ctx, actor, func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT INTO model_prices(model, input, output, cache_write_5m, cache_write_1h, cache_read, updated_at, updated_by)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(model) DO UPDATE SET input = excluded.input, output = excluded.output, cache_write_5m = excluded.cache_write_5m,
				cache_write_1h = excluded.cache_write_1h, cache_read = excluded.cache_read, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
			p.Model, p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead, now, nullUser(actor.id()))
		return err
	})
	if err != nil {
		return nil, err
	}
	at := msTime(now)
	p.UpdatedAt = &at
	return &p, nil
}

// DeleteModelPrice removes a price (the model's cost shows as unknown).
func (s *Service) DeleteModelPrice(ctx context.Context, actor *Actor, model string) error {
	if err := requireUsageAdmin(actor); err != nil {
		return err
	}
	model = strings.TrimSpace(model) // as SetModelPrice stores it
	return s.write(ctx, actor, func(t *tx) error {
		r, err := t.ExecContext(ctx, `DELETE FROM model_prices WHERE model = ?`, model)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return notFound("price")
		}
		return nil
	})
}

func requireUsageAdmin(actor *Actor) error {
	if actor == nil || actor.User == nil {
		return ErrUnauthorized
	}
	if err := actor.requireHuman("changing prices"); err != nil {
		return err
	}
	if !actor.isAdmin() {
		return forbidden("only admins change prices")
	}
	return actor.canWrite()
}
