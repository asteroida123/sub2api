package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// AccountNodeHealth 窗口猎手 P0：账号×出口节点的降智健康状态机。
//
// 每行代表一个 (账号, 出口节点) 组合的最新探测结论：
//   - state: unknown | full_power | degraded | cooldown（cooldown 为 degraded 且未过 cooldown_until 的派生显示态）
//   - 探测即污染：last_probe_at 记录每次探测时间，是复探排程纪律的依据。
type AccountNodeHealth struct {
	ent.Schema
}

func (AccountNodeHealth) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "account_node_health"},
	}
}

func (AccountNodeHealth) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (AccountNodeHealth) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id"),
		field.Int64("proxy_id").
			Default(0).
			Comment("出口代理 ID；0 = 直连出口（无代理）。"),
		field.String("region").
			MaxLen(64).
			Default("").
			Comment("出口地理快照（国家/区域，尽力填充）。"),
		field.String("state").
			MaxLen(20).
			Default("unknown").
			Comment("unknown | full_power | degraded | cooldown。"),
		field.Time("window_opened_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("最近一次命中满血的时间（乐观窗口起点）。"),
		field.Time("degraded_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("最近一次判定降智的时间。"),
		field.Time("cooldown_until").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("降智冷却截止时间；到期前该组合应保持静默。"),
		field.Int64("probe_count").
			Default(0).
			Comment("累计探测次数（题库轮换依据）。"),
		field.Time("last_probe_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("最近一次探测时间（含失败探测；探测即污染）。"),
		field.Text("last_probe_answer").
			Default("").
			Comment("最近一次指纹问题的上游原文回答（截断存储）。"),
	}
}

func (AccountNodeHealth) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "proxy_id").Unique(),
		index.Fields("account_id"),
		index.Fields("state"),
	}
}
