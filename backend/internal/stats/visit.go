package stats

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxVisitorIDLength 是前端随机标识的字节上限。
// 超过上限按「缺失」处理并退回 IP 摘要：客户端可以把标识填成任意长度，
// 不设上限等于让一次请求的摘要开销由调用方决定。
const maxVisitorIDLength = 128

// 匿名标识的两种来源，写进摘要消息里区分，避免同一个字符串在两种来源下撞出同一摘要。
const (
	visitorKindID = "id" // 前端本地随机标识
	visitorKindIP = "ip" // 加日盐的 IP 摘要兜底
)

// Visit 是一次已完成公开查询的记账入参：字段全部来自请求，不含响应内容与查询结果。
type Visit struct {
	VisitorID string // 前端随机匿名标识；空串表示缺失，退回 IP 摘要
	IP        string // 来源 IP，只在内存里参与摘要，不落库、不进日志
	Building  string // 请求筛选的教学楼编号；空串表示「全部」，不计楼维度
	RoomID    string // 全天状态视图的房间身份；空串表示本次不是单房查询
	Keyword   string // 请求的原始关键词，由记账侧归一化并截断
	StartNode string // 节次区间起点 01..12；空串表示本次没有节次区间
	EndNode   string // 节次区间终点 01..12
}

// NormalizeKeyword 把关键词折成统计口径的规范形态：去首尾与重复空白、全角转半角、转小写，
// 最后截断到 MaxKeywordRunes（Requirement 34.5、PRD 关键词榜单口径）。
// 归一化让「综合楼」「综合楼 」「ＳＺ」这类写法落到同一行；原始输入不落库（Requirement 34.9）。
func NormalizeKeyword(raw string) string {
	return truncateRunes(collapseSpaces(foldWidthAndCase(raw)), MaxKeywordRunes)
}

// NormalizeDimensionKey 把楼编号与房号夹成统计口径的维度键：只去首尾空白并按字符截断，
// 不折全半角也不转小写——楼编号与房号是上游标识，改写大小写会让统计行与白名单、room 表对不上。
// 两者都来自公开请求，长度不受信任：不夹取就会撞上统计日表的 btree 索引条目上限，
// 一条超长键足以让同批的其余维度一起写不进去（Requirement 34.5）。
// 空串表示本次没有该维度，由聚合侧跳过。
func NormalizeDimensionKey(raw string) string {
	return truncateRunes(strings.TrimSpace(raw), MaxDimensionRunes)
}

// foldWidthAndCase 把全角 ASCII 映射回半角、表意空格映射为普通空格，并把字母转小写。
func foldWidthAndCase(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\u3000':
			b.WriteByte(' ')
		case r >= '\uFF01' && r <= '\uFF5E':
			b.WriteRune(unicode.ToLower(r - 0xFEE0))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// collapseSpaces 把连续空白折成一个空格并去掉首尾空白。
func collapseSpaces(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// truncateRunes 按字符（而不是字节）截断，避免把一个多字节字符切成半个。
func truncateRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

// Hasher 把匿名标识折成不可逆摘要。零值可用，表示只按天加盐。
type Hasher struct {
	salt []byte // 部署级附加盐；为空时仍按天加盐
}

// NewHasher 组装摘要器。salt 为部署级密钥，当前装配层留空（秘密管理尚未落库），
// 摘要本身已包含业务日期，因此同一访客跨天不可关联。
func NewHasher(salt []byte) Hasher {
	return Hasher{salt: salt}
}

// Hash 计算一次访问的匿名摘要：sha256(version | 附加盐 | 日盐 | 来源类型 | 原始标识)。
// 前端随机标识优先；缺失或超长时退回 IP；两者都没有时返回空串，本次不计 UV。
// 业务日期参与摘要即「按天加盐」：同一标识换一天得到完全不同的摘要（Requirement 34.4）。
func (h Hasher) Hash(day time.Time, visitorID, ip string) string {
	kind, value := visitorIdentity(visitorID, ip)
	if kind == "" {
		return ""
	}

	digest := sha256.New()
	// 每段后面补一个 0 字节分隔，避免「ab」+「c」与「a」+「bc」拼出同一条消息。
	for _, part := range [][]byte{
		[]byte(hashVersion),
		h.salt,
		[]byte(dayOf(day).Format(time.DateOnly)),
		[]byte(kind),
		[]byte(value),
	} {
		digest.Write(part)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// visitorIdentity 决定用哪种标识做摘要，并做长度与空值兜底。
func visitorIdentity(visitorID, ip string) (string, string) {
	if id := strings.TrimSpace(visitorID); id != "" && len(id) <= maxVisitorIDLength {
		return visitorKindID, id
	}
	if addr := strings.TrimSpace(ip); addr != "" {
		return visitorKindIP, addr
	}
	return "", ""
}

// nodeRange 把 01..12 的封闭区间展开成小节编号序列，供节次分布逐节计数（Requirement 34.8）。
// 区间的合法性由公开查询校验；这里只做防御：解析不出或起点晚于终点时返回空，不猜区间。
func nodeRange(start, end string) []string {
	first, ok := nodeNumber(start)
	if !ok {
		return nil
	}
	last, ok := nodeNumber(end)
	if !ok || last < first {
		return nil
	}

	nodes := make([]string, 0, last-first+1)
	for number := first; number <= last; number++ {
		nodes = append(nodes, string([]byte{byte('0' + number/10), byte('0' + number%10)}))
	}
	return nodes
}

// nodeNumber 把两位小节编号转成序号；00、13 与非数字都判非法。
func nodeNumber(node string) (int, bool) {
	if len(node) != 2 {
		return 0, false
	}
	if node[0] < '0' || node[0] > '9' || node[1] < '0' || node[1] > '9' {
		return 0, false
	}
	number := int(node[0]-'0')*10 + int(node[1]-'0')
	if number < 1 || number > 12 {
		return 0, false
	}
	return number, true
}
