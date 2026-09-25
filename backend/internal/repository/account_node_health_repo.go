package repository

import (
	"context"
	"database/sql"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountnodehealth"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// accountNodeHealthRepository 窗口猎手 P0 健康状态机仓储。
type accountNodeHealthRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

func NewWindowProbeRepository(client *dbent.Client, sqlDB *sql.DB) service.WindowProbeRepository {
	return &accountNodeHealthRepository{client: client, sql: sqlDB}
}

func entityToService(m *dbent.AccountNodeHealth) *service.AccountNodeHealth {
	if m == nil {
		return nil
	}
	return &service.AccountNodeHealth{
		ID:              m.ID,
		AccountID:       m.AccountID,
		ProxyID:         m.ProxyID,
		Region:          m.Region,
		State:           m.State,
		WindowOpenedAt:  m.WindowOpenedAt,
		DegradedAt:      m.DegradedAt,
		CooldownUntil:   m.CooldownUntil,
		ProbeCount:      m.ProbeCount,
		LastProbeAt:     m.LastProbeAt,
		LastProbeAnswer: m.LastProbeAnswer,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}
}

func (r *accountNodeHealthRepository) GetByAccountAndProxy(ctx context.Context, accountID, proxyID int64) (*service.AccountNodeHealth, error) {
	m, err := r.client.AccountNodeHealth.Query().
		Where(
			accountnodehealth.AccountIDEQ(accountID),
			accountnodehealth.ProxyIDEQ(proxyID),
		).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrNodeHealthNotFound
		}
		return nil, err
	}
	return entityToService(m), nil
}

// Upsert 按 (account_id, proxy_id) 唯一键写入或更新。
func (r *accountNodeHealthRepository) Upsert(ctx context.Context, health *service.AccountNodeHealth) error {
	if health == nil {
		return nil
	}
	builder := r.client.AccountNodeHealth.Create().
		SetAccountID(health.AccountID).
		SetProxyID(health.ProxyID).
		SetRegion(health.Region).
		SetState(health.State).
		SetProbeCount(health.ProbeCount).
		SetLastProbeAnswer(health.LastProbeAnswer).
		SetNillableWindowOpenedAt(health.WindowOpenedAt).
		SetNillableDegradedAt(health.DegradedAt).
		SetNillableCooldownUntil(health.CooldownUntil).
		SetNillableLastProbeAt(health.LastProbeAt).
		OnConflictColumns(accountnodehealth.FieldAccountID, accountnodehealth.FieldProxyID).
		UpdateNewValues()
	created, err := builder.ID(ctx)
	if err != nil {
		return err
	}
	health.ID = created
	return nil
}

func (r *accountNodeHealthRepository) List(ctx context.Context) ([]*service.AccountNodeHealth, error) {
	rows, err := r.client.AccountNodeHealth.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*service.AccountNodeHealth, 0, len(rows))
	for _, m := range rows {
		out = append(out, entityToService(m))
	}
	return out, nil
}

func (r *accountNodeHealthRepository) ListByAccountIDs(ctx context.Context, accountIDs []int64) ([]*service.AccountNodeHealth, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	rows, err := r.client.AccountNodeHealth.Query().
		Where(accountnodehealth.AccountIDIn(accountIDs...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*service.AccountNodeHealth, 0, len(rows))
	for _, m := range rows {
		out = append(out, entityToService(m))
	}
	return out, nil
}
