package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// settingsQuerier 是设置仓储需要的最小能力：Query 一次读多行、Begin 开单事务。
// QueryRow 目前没有调用点（reserved：留给将来的单键读取），保留它是因为 design.md
// 把这个仓储的最小能力定为「多行读 + 单行读 + 开事务」这一组；将来加单键读取时
// 不必再动接口。
//
// 用窄接口而不是 *pgxpool.Pool，与 adminQuerier / releaseTxBeginner 的既有写法一致：
// 单元测试可以替身验证错误口径，装配时交同一个连接池即可。
type settingsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// settingsTx 是设置写入事务需要的最小能力：事务体只写不读，所以只有 Exec。
// 原子性由 pgx.Tx 的 Commit/Rollback 保证，扫描与判定都不在事务里做。
type settingsTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// loadSettingsSQL 一次取回七个业务设置键，避免七次往返。
//
// 键集合以数组参数传入（model.SettingKeys），SQL 里不写字面量 IN 列表：键集合只有
// 一个来源，漏掉一项时设置快照就读不出那个项，而调用点不会有任何编译错误。
// ANY($1) 让 pgx 把 []string 编成 text[]，键外行因此在库侧就被排除——管理端既看不到
// 也无法借读取路径探测其他行。
//
// ORDER BY key 只定顺序、不表达语义：快照组装按键查表，与顺序无关；排序是为了让
// 集成测试与排障看到稳定输出。updated_at 原样返回，不在 SQL 里写 now()：
// 时间口径由服务层传入（同一批写入用同一个 now），仓储只负责存取。
const loadSettingsSQL = `
SELECT key, value, is_secret, updated_at
  FROM settings
 WHERE key = ANY($1)
 ORDER BY key`

// upsertSettingSQL 覆盖写一个设置项。
//
// 用 ON CONFLICT (key) DO UPDATE 而不是 DO NOTHING：设置项的语义是「替换」，
// 冲突时必须把 value / is_secret / updated_at 三列都改成新值；DO NOTHING 会让
// 第二次保存静默不生效，管理员看到成功而库里还是旧值。
// updated_at 取 EXCLUDED.updated_at（即调用方传入的 now），不在 SQL 里写 now()：
// 同一批变更的时间戳必须一致，库侧时钟不参与这个口径。
const upsertSettingSQL = `
INSERT INTO settings (key, value, is_secret, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key) DO UPDATE
   SET value = EXCLUDED.value,
       is_secret = EXCLUDED.is_secret,
       updated_at = EXCLUDED.updated_at`

// deleteSettingSQL 删除一个设置项（局部更新里的「清除」语义）。
// 0 行不算错误：重复删除、键本来就没写过，结果都是「这个键不存在」，
// 与 AdminRepo.DeleteSession 的幂等口径一致，不该让清除路径报 500。
const deleteSettingSQL = `DELETE FROM settings WHERE key = $1`

// SettingsRepo 把 settings 表的读写 SQL 闭在内部。
//
// 它是 settings.Store 的结构匹配实现：store 不 import settings（依赖方向见
// backend/.golangci.yml），settings 在自己的包里声明消费侧接口，方法名与签名
// 与本类型一一对应。对应关系最终由装配处的编译期赋值确认，这里不复制接口。
type SettingsRepo struct {
	q settingsQuerier
}

// NewSettingsRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewSettingsRepo(q settingsQuerier) SettingsRepo {
	return SettingsRepo{q: q}
}

// LoadSettings 读回七个业务设置键的当前行，键外行不会出现在结果里。
//
// 返回的 SettingRow.Value 是库里的原样值：秘密项仍是 v1: 信封密文，解密只发生在
// settings.Service.Reveal，仓储不接触明文（CP1/CP2）。updated_at 也原样来自库，
// 不在这里补零、不换算时区。
//
// 没有命中任何行时返回空切片而不是 nil：调用方（快照组装）不需要为「新库」写
// 一条判空分支，也不会出现 nil 与空切片两种「没有设置」的表示。
// 取到一半失败（连接中断等）返回错误而不是部分结果：半套快照比失败更难排查，
// 所有失败都包 model.ErrSettingStoreFailed（design「哨兵的包装关系」）。
func (r SettingsRepo) LoadSettings(ctx context.Context) ([]model.SettingRow, error) {
	keys := model.SettingKeys()
	rows, err := r.q.Query(ctx, loadSettingsSQL, keys)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取设置失败: %w", model.ErrSettingStoreFailed, err)
	}
	defer rows.Close()

	found := make([]model.SettingRow, 0, len(keys))
	for rows.Next() {
		var row model.SettingRow
		if err := rows.Scan(&row.Key, &row.Value, &row.Secret, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("%w: 扫描设置行失败: %w", model.ErrSettingStoreFailed, err)
		}
		found = append(found, row)
	}
	// rows.Err() 覆盖「遍历到一半连接断了」：此时已取到的行是残的，
	// 不能当完整快照交给上层（Requirement 6.5 / CP5）。
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 读取设置失败: %w", model.ErrSettingStoreFailed, err)
	}
	return found, nil
}

// ApplySettings 在单个事务里逐条执行整批变更：Delete 走 DELETE，其余按键 upsert。
//
// 任意一个键不属于 model.SettingKeys() 时整批拒绝、一条不写、也不开事务（校验先于
// Begin）：读路径只认七个键，键外行写进去之后 LoadSettings 永远读不到，事后只能去翻库
// 排查（校验见 validateSettingKeys）。
//
// 任一条失败立即返回并整体回滚，库里不会留下「前几条生效、后几条没写」的半套设置
// （Requirement 6.4 / CP5）。逐条执行而不是拼一条多行 INSERT，是为了让删除与
// upsert 混在同一批（管理员一次保存里可能既有清除又有替换），同时保证顺序即
// 提交顺序——白名单这类顺序敏感的值将来也走这条路径。
//
// now 由服务层传入，同一批的每一条都用它写 updated_at：一次保存的所有行时间戳一致，
// 不随语句执行时刻漂移，也不读库侧时钟（design「时间一律取自 s.clock()」）。
//
// 空批次直接返回 nil、不开事务：没有行要写，开事务只是多一次往返，还会让
// 「零变更」在连接池不可用时表现成一次失败。
//
// 错误只带键名与哨兵，不带值：秘密项的值是密文，任何值都不该出现在错误消息里
// （CP1 的日志/响应脱敏口径）。
func (r SettingsRepo) ApplySettings(ctx context.Context, changes []model.SettingChange, now time.Time) error {
	if len(changes) == 0 {
		return nil
	}
	// 键白名单先于 Begin：越界键是编码错误，不该留下一行读不回来的设置，
	// 也不该为一次注定失败的写入多开一个事务。
	if err := validateSettingKeys(changes); err != nil {
		return err
	}

	tx, err := r.q.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: 设置事务开启失败: %w", model.ErrSettingStoreFailed, err)
	}
	// 提交成功后再回滚是空操作；所有失败路径都靠这里收尾（与 CommitRelease 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySettingChanges(ctx, tx, changes, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: 设置事务提交失败: %w", model.ErrSettingStoreFailed, err)
	}
	return nil
}

// validateSettingKeys 挡掉七个业务键之外的变更。
//
// 键集合仍然只有 model.SettingKeys() 一个来源，这里只是把同一份集合读成判断用的集合：
// 写路径若接受键外值，库里会出现一行 LoadSettings 永远读不到、也永远不会被这七个键的
// 清除路径删掉的行。校验放在开事务之前，失败即整批拒绝，错误里只带键名不带值（CP1）。
func validateSettingKeys(changes []model.SettingChange) error {
	allowed := make(map[string]struct{}, len(model.SettingKeys()))
	for _, key := range model.SettingKeys() {
		allowed[key] = struct{}{}
	}
	for _, change := range changes {
		if _, ok := allowed[change.Key]; !ok {
			return fmt.Errorf("%w: 设置键 %q 不在七个业务键之内", model.ErrSettingStoreFailed, change.Key)
		}
	}
	return nil
}

// applySettingChanges 是 ApplySettings 的事务体：只按提交顺序逐条写，不负责开事务、
// 提交与回滚。拆出来是为了让「顺序、失败即停、同一批同一个 now」能用一个记录型
// 事务替身单独验证，不必连库（真实 SQL 语义由集成测试覆盖）。
func applySettingChanges(ctx context.Context, tx settingsTx, changes []model.SettingChange, now time.Time) error {
	for _, change := range changes {
		if change.Delete {
			if _, err := tx.Exec(ctx, deleteSettingSQL, change.Key); err != nil {
				return fmt.Errorf("%w: 删除设置 %s 失败: %w", model.ErrSettingStoreFailed, change.Key, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, upsertSettingSQL, change.Key, change.Value, change.Secret, now); err != nil {
			return fmt.Errorf("%w: 写入设置 %s 失败: %w", model.ErrSettingStoreFailed, change.Key, err)
		}
	}
	return nil
}
