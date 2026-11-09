package settings

// 本文件是设置读写的服务层：把 Patch 的局部更新语义落到「一次事务」，把秘密项的加封与
// 解封收敛到一处，并组装可回显的快照。依赖方向见 design.md「依赖方向」：
// settings → model、secret（cronexpr 由同包 settings.go 使用），不 import store 与 api。
// 三个注入点都只为「可替身测试」存在：
//
//   - Store：设置仓储的消费侧接口，签名与 store.SettingsRepo 结构匹配；
//   - Sealer：secret.Manager 的窄接口，只取当前密钥，不持有它的其余能力；
//   - clock：同一批写入的 updated_at 用同一个时刻，测试可注入固定值。
//
// 秘密明文只有两个出现位置：Patch 入参（管理员提交的）与 Reveal 返回值（采集执行用）。
// 快照类型里没有能承载秘密明文的字段，因此回显由类型保证（CP1、Requirement 3.4）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
)

// Store 是设置仓储的消费侧接口（依赖倒置：接口定义在消费方）。
//
// 声明在这里而不是 import store，是为了让本包不依赖 SQL 与驱动，也让替身测试不必连库；
// 装配层用 var _ settings.Store = store.SettingsRepo{} 把两者的结构匹配钉在编译期。
// store 侧不 import 本包，因此这里的方法名与签名就是唯一契约。
type Store interface {
	// LoadSettings 一次读回七个业务设置键的当前行；键外行不会出现在结果里。
	// 秘密项的值是库里的密文，仓储不接触明文（CP1/CP2）。
	LoadSettings(ctx context.Context) ([]model.SettingRow, error)
	// ApplySettings 在单个事务里按给定顺序写入整批变更，任一条失败整体回滚。
	// now 由服务层传入，同一批的每一条都用它写 updated_at。
	ApplySettings(ctx context.Context, changes []model.SettingChange, now time.Time) error
}

// Sealer 是秘密加封与解封所需的最小能力：只取当前密钥。
//
// 取 `secret.Key` 而不是接受 Seal/Open 方法，是因为 AAD 必须由本包按
// 「settings/<key>」决定（Requirement 3.2）：把 AAD 让给注入方拼，AAD 口径就会散到
// 装配层，测试也无法断言密文绑定到了具体设置项（CP2）。Manager 的其余能力
// （Create/Reload/Path）与设置读写无关，窄接口也顺带挡住误用。
type Sealer interface {
	Current() (secret.Key, error)
}

// Snapshot 是可回显的全部设置：**没有秘密明文字段**，三项秘密只有「已设置」布尔。
//
// 这不是约定而是类型保证：handler 拿到 Snapshot 就拿不到可回显的秘密，哨兵字符串
// 不可能顺着响应流出去（CP1、Requirement 1.2 / 3.4）。Whitelist 恒为非 nil，
// 空名单序列化为 [] 而不是 null（Requirement 1.3）。
type Snapshot struct {
	JWUsername          string
	JWPasswordSet       bool
	OCRBaseURL          string
	FeishuWebhookSet    bool
	FeishuSignSecretSet bool
	CronExpr            string
	Whitelist           []model.Building
}

// Service 是设置的读写入口：一次 Patch 一次事务、一次回读。
//
// 它无状态（store/keys/clock 都在构造时定死），可以并发使用：Query 与事务的并发安全
// 由仓储保证，密钥读取由 Manager 的读锁保证，clock 只被调用、不被写。
type Service struct {
	store Store
	keys  Sealer
	clock func() time.Time
}

// New 组装一个设置服务。clock 为 nil 时退回 time.Now（只有测试需要注入固定时刻）。
//
// keys 允许为 nil：装配缺失时把它当成「密钥不可用」处理，而不是在第一次保存秘密时
// 空指针 panic。这样「密钥没装好」的请求仍然得到 50000 且零写入（CP3），
// 与 Manager 未 Load 时的表现一致。
func New(store Store, keys Sealer, clock func() time.Time) *Service {
	return &Service{store: store, keys: keys, clock: clock}
}

// Load 读七个键的当前行并组装快照，供 GET /api/admin/settings 与 Patch 的回读使用。
//
// 组装口径（Requirement 1.1–1.3）：
//   - 字符串项原样返回，缺行即空串；
//   - `*_set` 只由**行是否存在**决定：不看值是否为空，也不看 is_secret 列（CP4）。
//     手工插入一行 is_secret=false 的 jw_password 仍然是「已设置」；
//   - cron_expr 缺行回落 model.DefaultCronExpr（Requirement 9.5）；行存在就原样返回，
//     值非法不在这一层回落——那是调度器装载时的职责（Requirement 9.6）；
//   - building_whitelist 缺行回空数组，非法 JSON 也回空数组（与 store 侧 meta 的既有
//     口径一致：一条设置被手改成坏 JSON 不该让整个读接口 500，空名单本身已有明确语义）。
//
// 读失败原样上抛：仓储已经用 %w 包好 model.ErrSettingStoreFailed，本层不改写错误类别。
func (s *Service) Load(ctx context.Context) (Snapshot, error) {
	rows, err := s.store.LoadSettings(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromRows(rows), nil
}

// Patch 执行一次局部更新：Validate → 加封非空秘密 → 一次 ApplySettings → 回读。
//
// 顺序是固定的，且每一段都有理由（design「管理 API 契约」的约束段）：
//  1. Validate 先跑：非法请求不必碰密钥文件，也不必开事务（Requirement 6.3、CP5）；
//  2. 秘密字段的加封全部发生在写库之前：拿不到密钥时整请求失败、零行写入，
//     不存在「密钥坏了就存明文」或「写了一半秘密」的退化路径（CP3、Requirement 3.3）；
//     只有把秘密设为空串（清除）时不取密钥（Requirement 3.6）；
//  3. 整批变更放进**一次** ApplySettings：仓储用单事务保证任一条失败整体回滚（Requirement 6.4）；
//  4. 回读：响应与 GET 同形状，前端保存后不必再发一次 GET（Requirement 2.7）。
//
// 第 4 步失败返回 *ReadBackError 而不是裸仓储错误：此时写入已提交，与第 3 步失败
// （事务已回滚）是两种事实，上层要能把它们记成两种日志文案（CP5）。这条包装只在
// 真的开过写事务（changes 非空）时成立：空补丁没有「已提交」可言，它的回读失败
// 按普通读失败原样上抛。
//
// 省略全部字段（Patch{}）时不调 ApplySettings：没有变更就没有事务可开，直接回读。
// 秘密的明文不 trim、非秘密文本 trim 后存、白名单按提交顺序序列化，各自的口径见
// secretChange / textChange / whitelistChange。
func (s *Service) Patch(ctx context.Context, p Patch) (Snapshot, error) {
	if err := Validate(p); err != nil {
		return Snapshot{}, err
	}

	// 一次保存里三个秘密共用同一把密钥：惰性取，只有在真的出现非空秘密字段时才调
	// Current()，且只调一次（同一次加密不该看到两把不同的密钥）。
	var (
		currentKey secret.Key
		keyLoaded  bool
	)
	loadKey := func() (secret.Key, error) {
		if keyLoaded {
			return currentKey, nil
		}
		if s.keys == nil {
			return secret.Key{}, model.ErrKeyUnavailable
		}
		key, err := s.keys.Current()
		if err != nil {
			return secret.Key{}, err
		}
		currentKey, keyLoaded = key, true
		return key, nil
	}

	changes := make([]model.SettingChange, 0, len(model.SettingKeys()))

	if p.JWUsername != nil {
		changes = append(changes, textChange(model.SettingJWUsername, *p.JWUsername))
	}
	if p.JWPassword != nil {
		change, err := secretChange(loadKey, model.SettingJWPassword, *p.JWPassword)
		if err != nil {
			return Snapshot{}, err
		}
		changes = append(changes, change)
	}
	if p.OCRBaseURL != nil {
		changes = append(changes, textChange(model.SettingOCRBaseURL, *p.OCRBaseURL))
	}
	if p.FeishuWebhook != nil {
		change, err := secretChange(loadKey, model.SettingFeishuWebhook, *p.FeishuWebhook)
		if err != nil {
			return Snapshot{}, err
		}
		changes = append(changes, change)
	}
	if p.FeishuSignSecret != nil {
		change, err := secretChange(loadKey, model.SettingFeishuSignSecret, *p.FeishuSignSecret)
		if err != nil {
			return Snapshot{}, err
		}
		changes = append(changes, change)
	}
	if p.CronExpr != nil {
		// Validate 已经拒绝 trim 后为空的表达式（Requirement 5.3），所以这里不再判空、
		// 也不走 textChange 的删除分支：cron_expr 没有「清除」语义，只有替换。
		changes = append(changes, model.SettingChange{
			Key:   model.SettingCronExpr,
			Value: strings.TrimSpace(*p.CronExpr),
		})
	}
	if p.Whitelist != nil {
		change, err := whitelistChange(*p.Whitelist)
		if err != nil {
			return Snapshot{}, err
		}
		changes = append(changes, change)
	}

	if len(changes) > 0 {
		if err := s.store.ApplySettings(ctx, changes, s.now()); err != nil {
			return Snapshot{}, err
		}
	}

	snapshot, err := s.Load(ctx)
	if err != nil {
		// 只有真的写过库才有「写事务已经提交」这层事实。空补丁（PUT {} / PUT null）
		// 从不开事务，回读失败就是普通读失败：包成 ReadBackError 会让日志谎称
		// 「写入已提交」，而实际上一个键都没写（CP5）。
		//
		// 有变更时，包一层独立类型，让 API 层能把「回读失败（写入已提交）」与
		// 「写入失败（事务已回滚）」映射成两种日志文案：两者对客户端都是 500/50000，
		// 但排障时混成一句会让人以为设置没存进去（CP5）。
		if len(changes) > 0 {
			return Snapshot{}, &ReadBackError{Err: err}
		}
		return Snapshot{}, err
	}
	return snapshot, nil
}

// ReadBackError 表示一次 Patch 的写事务已经提交、但提交后的回读失败。
//
// 它只覆盖「本次确实开了写事务」的情况：空补丁（PUT {} / PUT null）不写库，回读
// 失败就是普通读失败，不能被这个类型认领，否则「已提交」这句日志会是假的。
//
// 它与写入失败是两种事实：写入失败时事务已回滚，库里的设置与保存前逐字节相同；
// 回读失败时设置已经落库，只是 handler 拿不到最新快照（因此也没有表达式可交给调度器）。
// 客户端拿到的都是 500/50000 与固定文案，区别只在服务端日志：把回读失败记成
// 「保存设置失败」会导致多余的重放甚至误判「设置丢了」（design 错误表对应行、CP5）。
//
// Err 是回读时仓储返回的原始错误。仓储契约保证它已经用 %w 包着
// model.ErrSettingStoreFailed，所以 errors.Is(err, model.ErrSettingStoreFailed) 仍然成立；
// 本类型只负责把「已提交」这一事实显式化，供上层用 errors.As 分流。
type ReadBackError struct {
	Err error
}

// Error 在原始错误前加上「已提交」，不改变原始错误的类别文案。
func (e *ReadBackError) Error() string {
	if e.Err == nil {
		return "设置已提交但回读失败"
	}
	return "设置已提交但回读失败: " + e.Err.Error()
}

// Unwrap 透传原始错误：errors.Is / errors.As 对它的判定与直接返回仓储错误时一致。
func (e *ReadBackError) Unwrap() error { return e.Err }

// Reveal 解封一个秘密项并返回明文，是唯一的明文出口（collect-runner 与测试用；
// 名字刻意可 grep）。快照与 handler 都拿不到它。
//
// 四种失败都返回错误而不是空值：键不是秘密项、该键未设置、密钥不可用、解封失败
// （AAD 不符、密文被篡改、版本前缀不认）。Requirement 3.5 要求「解封失败返回错误，
// 不以空值继续」——拿着空密码去登录只会把失败推到上游，排查时看不出真正原因。
//
// AAD 与写入侧共用 settingAAD，换键名的密文解不开（CP2）。明文只出现在返回值里，
// 错误消息只带键名：明文与密文都不进日志（CP1）。
func (s *Service) Reveal(ctx context.Context, key string) (string, error) {
	if !model.SecretSetting(key) {
		return "", fmt.Errorf("设置项 %s 不是秘密项，不能解封", key)
	}

	rows, err := s.store.LoadSettings(ctx)
	if err != nil {
		return "", err
	}
	row, found := findRow(rows, key)
	if !found {
		return "", fmt.Errorf("设置项 %s 未设置", key)
	}

	if s.keys == nil {
		return "", model.ErrKeyUnavailable
	}
	current, err := s.keys.Current()
	if err != nil {
		return "", err
	}

	plaintext, err := current.Open(settingAAD(key), row.Value)
	if err != nil {
		return "", fmt.Errorf("解封设置项 %s 失败: %w", key, err)
	}
	return string(plaintext), nil
}

// Whitelist 返回当前白名单，供 runner 判空（空名单要建立 blocked 轮次）。
//
// 顺序即库里的提交顺序（D11：顺序即归属裁决顺序），这里不排序、不过滤。
func (s *Service) Whitelist(ctx context.Context) ([]model.Building, error) {
	snapshot, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.Whitelist, nil
}

// CronExpr 返回当前 cron 表达式，供调度器装载；该键缺行时回 model.DefaultCronExpr
// （Requirement 9.5）。
//
// 行存在但值非法（或为空串）时原样返回，不在这一层回落：Requirement 9.6 要求
// 「记一条 ERROR 后回落默认值」，那是调度器装载时的职责（scheduler）。服务层若也回落，
// 同一条坏数据就有两份处理口径，管理员也看不到库里到底存的是什么。
func (s *Service) CronExpr(ctx context.Context) (string, error) {
	snapshot, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	return snapshot.CronExpr, nil
}

// settingAAD 返回某个设置键的信封 AAD，固定为 "settings/<key>"。
//
// 写入（Seal）与读取（Open）都必须走这一个函数：两边各写一份字面量，迟早会有一边
// 改了另一边没改，那时密文就再也解不开了。AAD 把密文绑定到具体键，把 A 项的密文
// 挪到 B 项会因 AAD 不符解不开（Requirement 3.2、CP2）。
func settingAAD(key string) string {
	return "settings/" + key
}

// snapshotFromRows 把库里的七行（可能缺行、可能含键外行）组装成快照。
// 键外行一律忽略：读路径只认七个业务键（Requirement 1.1）。
func snapshotFromRows(rows []model.SettingRow) Snapshot {
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Key] = row.Value
	}

	snapshot := Snapshot{
		JWUsername: values[model.SettingJWUsername],
		OCRBaseURL: values[model.SettingOCRBaseURL],
		CronExpr:   model.DefaultCronExpr,
	}

	// `*_set` 只看行在不在：值是否为空、is_secret 列写的是什么都不参与判定
	// （Requirement 3.4、CP4）。两个返回值的第一个是值，第二个才是「存在」。
	_, snapshot.JWPasswordSet = values[model.SettingJWPassword]
	_, snapshot.FeishuWebhookSet = values[model.SettingFeishuWebhook]
	_, snapshot.FeishuSignSecretSet = values[model.SettingFeishuSignSecret]

	// 只有行存在才覆盖默认值：缺行回默认（Requirement 9.5），存在就照实返回。
	if expr, found := values[model.SettingCronExpr]; found {
		snapshot.CronExpr = expr
	}

	snapshot.Whitelist = decodeWhitelist(values[model.SettingBuildingWhitelist])
	return snapshot
}

// decodeWhitelist 把 building_whitelist 的 JSON 数组还原成条目，缺行、空串、字面量
// null 与非法 JSON 一律回空数组。
//
// 返回值恒为非 nil，让响应序列化成 [] 而不是 null（Requirement 1.3）。坏 JSON 回空
// 名单而不是报错，与 store 侧 metaBuildingWhitelist 的既有口径一致：公开读接口不能
// 因为一条设置被手改成坏 JSON 就整体 500，空名单本身有明确语义（采集受阻）。
func decodeWhitelist(raw string) []model.Building {
	if strings.TrimSpace(raw) == "" {
		return []model.Building{}
	}

	var entries []model.Building
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return []model.Building{}
	}
	if entries == nil {
		// 字面量 null 与 [] 都表示空名单，统一成非 nil 空切片。
		return []model.Building{}
	}
	return entries
}

// textChange 把非秘密文本项的一次提交翻成变更：trim 后为空表示清除（Delete），
// 否则存 trim 后的值。
//
// trim 后的值才落库，校验、存储与回显看到的就是同一串（Requirement 2.4、
// design「设置项」的处理顺序：先 TrimSpace，再校验与计数，最后存 trim 后的值）。
func textChange(key, raw string) model.SettingChange {
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return model.SettingChange{Key: key, Value: trimmed}
	}
	return model.SettingChange{Key: key, Delete: true}
}

// secretChange 把秘密项的一次提交翻成变更：恰好空串表示清除（Delete，不取密钥），
// 否则取当前密钥加封，落库的是 "v1:" 信封且 is_secret 记 true。
//
// 明文不 trim：密码可能以空格结尾，trim 会悄悄改掉管理员提交的凭据（design「设置项」）。
// 加封失败不返回半条变更，调用方整请求失败——秘密要么完整写入，要么一行都不写（CP3）。
// 错误里只出现键名：明文与密文都不进错误消息（CP1）。
func secretChange(loadKey func() (secret.Key, error), key, plaintext string) (model.SettingChange, error) {
	if plaintext == "" {
		return model.SettingChange{Key: key, Delete: true}, nil
	}

	current, err := loadKey()
	if err != nil {
		return model.SettingChange{}, err
	}
	envelope, err := current.Seal(settingAAD(key), []byte(plaintext))
	if err != nil {
		return model.SettingChange{}, fmt.Errorf("加密设置项 %s 失败: %w", key, err)
	}
	return model.SettingChange{Key: key, Value: envelope, Secret: true}, nil
}

// whitelistChange 先按提交顺序归一化（只 trim 每条的两个字段），再 json.Marshal 落库。
//
// 不排序、不去重后重排：白名单顺序即归属裁决顺序，保存时排序会悄悄改变裁决结果（D11）。
// 空数组合法，序列化后是 "[]"，不是删除该键（Requirement 4.5、CP7）。
// 归一化后的切片恒为非 nil，所以 `*[]model.Building(nil)` 也序列化成 []。
func whitelistChange(entries []model.Building) (model.SettingChange, error) {
	normalized := make([]model.Building, 0, len(entries))
	for _, building := range entries {
		normalized = append(normalized, model.Building{
			ID:   strings.TrimSpace(building.ID),
			Name: strings.TrimSpace(building.Name),
		})
	}

	raw, err := json.Marshal(normalized)
	if err != nil {
		// model.Building 只有两个 string 字段，Marshal 实际不会失败；保留这条分支是为了
		// 将来给它加字段（时间、接口等）时错误不会被静默当成空值写进库。
		return model.SettingChange{}, fmt.Errorf("序列化教学楼白名单失败: %w", err)
	}
	return model.SettingChange{Key: model.SettingBuildingWhitelist, Value: string(raw)}, nil
}

// findRow 在行集合里按键找一行；settings 的主键保证至多一行。
func findRow(rows []model.SettingRow, key string) (model.SettingRow, bool) {
	for _, row := range rows {
		if row.Key == key {
			return row, true
		}
	}
	return model.SettingRow{}, false
}

// now 返回本次写入使用的时刻。clock 为 nil 时退回 time.Now：同一批的每一条都复用
// 同一个返回值，updated_at 不随语句执行时刻漂移（design「时间一律取自 s.clock()」）。
func (s *Service) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}
