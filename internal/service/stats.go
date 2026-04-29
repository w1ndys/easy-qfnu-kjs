package service

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"

	_ "modernc.org/sqlite"
)

// StatsService 查询统计服务
type StatsService struct {
	db *sql.DB
	mu sync.Mutex // 保护 SQLite 串行写入
}

// NewStatsService 创建统计服务，打开或创建 SQLite 数据库
func NewStatsService(dbPath string) (*StatsService, error) {
	absPath, err := filepath.Abs(dbPath)
	if err == nil {
		dbPath = absPath
	}

	// 确保目录存在
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	if err := verifyWritable(dir); err != nil {
		return nil, fmt.Errorf("数据目录不可写: %w", err)
	}

	logger.Info("统计服务启动检查: db=%s dir=%s", dbPath, dir)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 关键配置：启用 WAL 模式，允许读写并发
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 WAL 模式失败: %w", err)
	}

	// 设置 busy_timeout，当数据库被锁时等待 5 秒而非立即报错
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}

	// 限制连接池：SQLite 是文件级数据库，多连接写入会导致锁冲突
	// 设置最大打开连接数为 1，确保写入串行化
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// 执行迁移
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	logger.Info("统计服务已初始化，数据库路径: %s", dbPath)
	return &StatsService{db: db}, nil
}

func verifyWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("读取目录信息失败: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}

	testFile, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("目录=%s mode=%s, 创建临时文件失败: %w", dir, info.Mode().Perm(), err)
	}

	fileName := testFile.Name()
	if closeErr := testFile.Close(); closeErr != nil {
		logger.Warn("关闭统计目录写入检测文件失败: %v", closeErr)
	}
	if removeErr := os.Remove(fileName); removeErr != nil {
		logger.Warn("清理统计目录写入检测文件失败: %v", removeErr)
	}

	logger.Info("统计目录写入检查通过: dir=%s mode=%s", dir, strings.TrimPrefix(info.Mode().String(), "d"))
	return nil
}

// migrateSchema 检测并迁移表结构
func migrateSchema(db *sql.DB) error {
	// 检测表是否存在
	var tableExists int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='query_logs'").Scan(&tableExists)
	if err != nil {
		return fmt.Errorf("检测表存在失败: %w", err)
	}

	if tableExists == 0 {
		// 表不存在，直接创建新表
		return createNewTable(db)
	}

	// 检测表是否包含 result_count 列（v3 新结构标志）
	var hasResultCount int
	err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('query_logs') WHERE name='result_count'").Scan(&hasResultCount)
	if err != nil {
		return fmt.Errorf("检测表结构失败: %w", err)
	}

	if hasResultCount == 0 {
		// 旧表结构（无论是 v1 classroom 版还是 v2 keyword-only 版），都需要迁移
		logger.Warn("检测到旧版 query_logs 表结构，开始迁移到 v3...")
		return migrateFromOldSchema(db)
	}

	// 新结构，增量添加 ip / ua_hash 列（v4 用户识别字段）
	if err := addUserColumnsIfNotExist(db); err != nil {
		return err
	}

	// 新表结构，检查索引
	return createIndexesIfNotExist(db)
}

// createNewTable 创建新的表结构
func createNewTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS query_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			keyword TEXT NOT NULL,
			date_offset INTEGER NOT NULL DEFAULT 0,
			start_node TEXT NOT NULL DEFAULT '',
			end_node TEXT NOT NULL DEFAULT '',
			result_count INTEGER NOT NULL DEFAULT 0,
			ip TEXT NOT NULL DEFAULT '',
			ua_hash TEXT NOT NULL DEFAULT '',
			queried_at DATETIME DEFAULT (datetime('now', 'localtime'))
		);
		CREATE INDEX IF NOT EXISTS idx_queried_at ON query_logs(queried_at);
		CREATE INDEX IF NOT EXISTS idx_keyword ON query_logs(keyword);
		CREATE INDEX IF NOT EXISTS idx_combo ON query_logs(keyword, date_offset, start_node, end_node, result_count);
		CREATE INDEX IF NOT EXISTS idx_ip_ua ON query_logs(ip, ua_hash);
	`)
	if err != nil {
		return fmt.Errorf("创建表失败: %w", err)
	}
	return nil
}

// addUserColumnsIfNotExist 为已有 v3 表增量添加 ip / ua_hash 列 (v4 迁移)
func addUserColumnsIfNotExist(db *sql.DB) error {
	// 检测 ip 列
	var hasIP int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('query_logs') WHERE name='ip'").Scan(&hasIP); err != nil {
		return fmt.Errorf("检测 ip 列失败: %w", err)
	}
	if hasIP == 0 {
		if _, err := db.Exec("ALTER TABLE query_logs ADD COLUMN ip TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("添加 ip 列失败: %w", err)
		}
		logger.Warn("已为 query_logs 添加 ip 列 (v4)")
	}

	// 检测 ua_hash 列
	var hasUA int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('query_logs') WHERE name='ua_hash'").Scan(&hasUA); err != nil {
		return fmt.Errorf("检测 ua_hash 列失败: %w", err)
	}
	if hasUA == 0 {
		if _, err := db.Exec("ALTER TABLE query_logs ADD COLUMN ua_hash TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("添加 ua_hash 列失败: %w", err)
		}
		logger.Warn("已为 query_logs 添加 ua_hash 列 (v4)")
	}

	return nil
}

// migrateFromOldSchema 从旧结构迁移到新结构（直接删除旧数据重建）
func migrateFromOldSchema(db *sql.DB) error {
	// 删除旧表
	_, err := db.Exec("DROP TABLE query_logs")
	if err != nil {
		return fmt.Errorf("删除旧表失败: %w", err)
	}

	// 创建新表
	err = createNewTable(db)
	if err != nil {
		return err
	}

	logger.Warn("旧版 query_logs 表已删除，已创建 v3 新表结构")
	return nil
}

// createIndexesIfNotExist 创建索引（如果不存在）
func createIndexesIfNotExist(db *sql.DB) error {
	_, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_queried_at ON query_logs(queried_at)")
	if err != nil {
		return fmt.Errorf("创建 queried_at 索引失败: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_keyword ON query_logs(keyword)")
	if err != nil {
		return fmt.Errorf("创建 keyword 索引失败: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_combo ON query_logs(keyword, date_offset, start_node, end_node, result_count)")
	if err != nil {
		return fmt.Errorf("创建 combo 索引失败: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_ip_ua ON query_logs(ip, ua_hash)")
	if err != nil {
		return fmt.Errorf("创建 ip_ua 索引失败: %w", err)
	}
	return nil
}

// RecordQuery 记录一次搜索查询（异步调用），包含完整的搜索参数、结果数量和用户识别信息
func (s *StatsService) RecordQuery(record model.QueryRecord) {
	if record.Keyword == "" {
		return
	}

	// 使用互斥锁确保写入串行化，防止 SQLite 并发写入冲突
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO query_logs (keyword, date_offset, start_node, end_node, result_count, ip, ua_hash) VALUES (?, ?, ?, ?, ?, ?, ?)",
		record.Keyword, record.DateOffset, record.StartNode, record.EndNode, record.ResultCount, record.IP, record.UAHash,
	)
	if err != nil {
		logger.Warn("记录搜索查询失败: %v", err)
	}
}

// GetStats 获取统计数据
func (s *StatsService) GetStats() (*model.StatsResponse, error) {
	now := time.Now()
	todayStart := now.Format("2006-01-02") + " 00:00:00"

	// 本周一
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	mondayDate := now.AddDate(0, 0, -(weekday - 1))
	weekStart := mondayDate.Format("2006-01-02") + " 00:00:00"

	// 本月1号
	monthStart := now.Format("2006-01") + "-01 00:00:00"

	resp := &model.StatsResponse{}

	// 今日查询次数
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", todayStart).Scan(&resp.TodayCount); err != nil {
		logger.Error("查询今日统计失败: %v", err)
		return nil, fmt.Errorf("查询今日统计失败: %w", err)
	}

	// 本周查询次数
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", weekStart).Scan(&resp.WeekCount); err != nil {
		logger.Error("查询本周统计失败: %v", err)
		return nil, fmt.Errorf("查询本周统计失败: %w", err)
	}

	// 本月查询次数
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", monthStart).Scan(&resp.MonthCount); err != nil {
		logger.Error("查询本月统计失败: %v", err)
		return nil, fmt.Errorf("查询本月统计失败: %w", err)
	}

	// 今日最热搜索关键词
	if err := s.db.QueryRow(
		"SELECT keyword FROM query_logs WHERE queried_at >= ? GROUP BY keyword ORDER BY COUNT(*) DESC LIMIT 1",
		todayStart,
	).Scan(&resp.TodayTop); err != nil && err != sql.ErrNoRows {
		logger.Error("查询今日最热关键词失败: %v", err)
		return nil, fmt.Errorf("查询今日最热关键词失败: %w", err)
	}

	// 本周最热搜索关键词
	if err := s.db.QueryRow(
		"SELECT keyword FROM query_logs WHERE queried_at >= ? GROUP BY keyword ORDER BY COUNT(*) DESC LIMIT 1",
		weekStart,
	).Scan(&resp.WeekTop); err != nil && err != sql.ErrNoRows {
		logger.Error("查询本周最热关键词失败: %v", err)
		return nil, fmt.Errorf("查询本周最热关键词失败: %w", err)
	}

	// 本月最热搜索关键词
	if err := s.db.QueryRow(
		"SELECT keyword FROM query_logs WHERE queried_at >= ? GROUP BY keyword ORDER BY COUNT(*) DESC LIMIT 1",
		monthStart,
	).Scan(&resp.MonthTop); err != nil && err != sql.ErrNoRows {
		logger.Error("查询本月最热关键词失败: %v", err)
		return nil, fmt.Errorf("查询本月最热关键词失败: %w", err)
	}

	return resp, nil
}

// GetTopQueries 获取搜索排行前 N 的查询组合（仅统计结果非空的记录）
func (s *StatsService) GetTopQueries(limit int) ([]model.TopQueryItem, error) {
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.db.Query(
		`SELECT keyword, date_offset, start_node, end_node, COUNT(*) AS cnt
		 FROM query_logs
		 WHERE result_count > 0
		 GROUP BY keyword, date_offset, start_node, end_node
		 ORDER BY cnt DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("查询热门搜索组合失败: %w", err)
	}
	defer rows.Close()

	var queries []model.TopQueryItem
	for rows.Next() {
		var item model.TopQueryItem
		if err := rows.Scan(&item.Building, &item.DateOffset, &item.StartNode, &item.EndNode, &item.Count); err != nil {
			return nil, fmt.Errorf("扫描热门搜索组合数据失败: %w", err)
		}
		queries = append(queries, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历热门搜索组合结果失败: %w", err)
	}

	return queries, nil
}

// GetDashboardData 获取数据大屏综合统计数据
func (s *StatsService) GetDashboardData(timeRange string, days int) (*model.DashboardResponse, error) {
	now := time.Now()
	var startTime string

	switch timeRange {
	case "today":
		startTime = now.Format("2006-01-02") + " 00:00:00"
	case "week":
		startTime = now.AddDate(0, 0, -6).Format("2006-01-02") + " 00:00:00"
	case "month":
		startTime = now.AddDate(0, 0, -29).Format("2006-01-02") + " 00:00:00"
	case "custom":
		if days < 1 {
			days = 1
		}
		startTime = now.AddDate(0, 0, -(days-1)).Format("2006-01-02") + " 00:00:00"
	default:
		startTime = now.Format("2006-01-02") + " 00:00:00"
	}

	resp := &model.DashboardResponse{}

	// 并行获取各项数据
	var overviewErr, trendErr, keywordErr, nodeErr, resultErr, hourlyErr error
	var wg sync.WaitGroup

	wg.Add(6)

	go func() {
		defer wg.Done()
		resp.Overview, overviewErr = s.getDashboardOverview(startTime)
	}()

	go func() {
		defer wg.Done()
		resp.Trend, trendErr = s.getDashboardTrend(timeRange, startTime, now, days)
	}()

	go func() {
		defer wg.Done()
		resp.TopKeywords, keywordErr = s.getDashboardKeywords(startTime)
	}()

	go func() {
		defer wg.Done()
		resp.NodeDist, nodeErr = s.getDashboardNodeDist(startTime)
	}()

	go func() {
		defer wg.Done()
		resp.ResultStats, resultErr = s.getDashboardResultStats(startTime)
	}()

	go func() {
		defer wg.Done()
		resp.HourlyDist, hourlyErr = s.getDashboardHourlyDist(startTime)
	}()

	wg.Wait()

	// 返回第一个遇到的错误
	for _, err := range []error{overviewErr, trendErr, keywordErr, nodeErr, resultErr, hourlyErr} {
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// getDashboardOverview 获取总览数据
func (s *StatsService) getDashboardOverview(startTime string) (model.DashboardOverview, error) {
	var o model.DashboardOverview
	now := time.Now()

	// 时间段内总查询次数 + 独立搜索词数 + 平均结果数 + 最大结果数
	err := s.db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT keyword),
			   COALESCE(AVG(result_count), 0), COALESCE(MAX(result_count), 0)
		FROM query_logs WHERE queried_at >= ?`, startTime,
	).Scan(&o.TotalCount, &o.UniqueKeywords, &o.AvgResultCount, &o.MaxResultCount)
	if err != nil {
		return o, fmt.Errorf("查询总览数据失败: %w", err)
	}

	// 时间段内独立用户数 (IP+UA 组合去重) 和独立 IP 数
	// 仅统计有 IP 记录的行 (兼容 v3 旧数据 ip 为空的情况)
	err = s.db.QueryRow(`
		SELECT COUNT(DISTINCT ip || '|' || ua_hash), COUNT(DISTINCT ip)
		FROM query_logs WHERE queried_at >= ? AND ip != ''`, startTime,
	).Scan(&o.UniqueVisitors, &o.UniqueIPs)
	if err != nil {
		return o, fmt.Errorf("查询独立用户数失败: %w", err)
	}

	// 今日
	todayStart := now.Format("2006-01-02") + " 00:00:00"
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", todayStart).Scan(&o.TodayCount); err != nil {
		return o, fmt.Errorf("查询今日统计失败: %w", err)
	}

	// 本周
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	weekStart := now.AddDate(0, 0, -(weekday-1)).Format("2006-01-02") + " 00:00:00"
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", weekStart).Scan(&o.WeekCount); err != nil {
		return o, fmt.Errorf("查询本周统计失败: %w", err)
	}

	// 本月
	monthStart := now.Format("2006-01") + "-01 00:00:00"
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_logs WHERE queried_at >= ?", monthStart).Scan(&o.MonthCount); err != nil {
		return o, fmt.Errorf("查询本月统计失败: %w", err)
	}

	return o, nil
}

// getDashboardTrend 获取趋势数据
func (s *StatsService) getDashboardTrend(timeRange, startTime string, now time.Time, days int) ([]model.TrendPoint, error) {
	var query string
	var points []model.TrendPoint

	switch timeRange {
	case "today":
		// 按小时分组
		query = `SELECT strftime('%H', queried_at) AS label, COUNT(*) AS cnt
				 FROM query_logs WHERE queried_at >= ?
				 GROUP BY label ORDER BY label`
		rows, err := s.db.Query(query, startTime)
		if err != nil {
			return nil, fmt.Errorf("查询今日趋势失败: %w", err)
		}
		defer rows.Close()

		hourMap := make(map[string]int)
		for rows.Next() {
			var label string
			var count int
			if err := rows.Scan(&label, &count); err != nil {
				return nil, err
			}
			hourMap[label] = count
		}
		// 填充 0-23 小时
		for h := 0; h < 24; h++ {
			label := fmt.Sprintf("%02d:00", h)
			key := fmt.Sprintf("%02d", h)
			points = append(points, model.TrendPoint{Label: label, Count: hourMap[key]})
		}

	case "week":
		// 按天分组，最近 7 天
		query = `SELECT strftime('%Y-%m-%d', queried_at) AS label, COUNT(*) AS cnt
				 FROM query_logs WHERE queried_at >= ?
				 GROUP BY label ORDER BY label`
		rows, err := s.db.Query(query, startTime)
		if err != nil {
			return nil, fmt.Errorf("查询本周趋势失败: %w", err)
		}
		defer rows.Close()

		dayMap := make(map[string]int)
		for rows.Next() {
			var label string
			var count int
			if err := rows.Scan(&label, &count); err != nil {
				return nil, err
			}
			dayMap[label] = count
		}
		// 填充最近 7 天
		for i := 6; i >= 0; i-- {
			d := now.AddDate(0, 0, -i)
			label := d.Format("01-02")
			key := d.Format("2006-01-02")
			points = append(points, model.TrendPoint{Label: label, Count: dayMap[key]})
		}

	case "month":
		// 按天分组，最近 30 天
		query = `SELECT strftime('%Y-%m-%d', queried_at) AS label, COUNT(*) AS cnt
				 FROM query_logs WHERE queried_at >= ?
				 GROUP BY label ORDER BY label`
		rows, err := s.db.Query(query, startTime)
		if err != nil {
			return nil, fmt.Errorf("查询本月趋势失败: %w", err)
		}
		defer rows.Close()

		dayMap := make(map[string]int)
		for rows.Next() {
			var label string
			var count int
			if err := rows.Scan(&label, &count); err != nil {
				return nil, err
			}
			dayMap[label] = count
		}
		// 填充最近 30 天
		for i := 29; i >= 0; i-- {
			d := now.AddDate(0, 0, -i)
			label := d.Format("01-02")
			key := d.Format("2006-01-02")
			points = append(points, model.TrendPoint{Label: label, Count: dayMap[key]})
		}

	case "custom":
		if days < 1 {
			days = 1
		}
		query = `SELECT strftime('%Y-%m-%d', queried_at) AS label, COUNT(*) AS cnt
				 FROM query_logs WHERE queried_at >= ?
				 GROUP BY label ORDER BY label`
		rows, err := s.db.Query(query, startTime)
		if err != nil {
			return nil, fmt.Errorf("查询自定义范围趋势失败: %w", err)
		}
		defer rows.Close()

		dayMap := make(map[string]int)
		for rows.Next() {
			var label string
			var count int
			if err := rows.Scan(&label, &count); err != nil {
				return nil, err
			}
			dayMap[label] = count
		}
		for i := days - 1; i >= 0; i-- {
			d := now.AddDate(0, 0, -i)
			label := d.Format("01-02")
			key := d.Format("2006-01-02")
			points = append(points, model.TrendPoint{Label: label, Count: dayMap[key]})
		}

	default:
		return nil, fmt.Errorf("不支持的时间范围: %s", timeRange)
	}

	return points, nil
}

// getDashboardKeywords 获取搜索词排行
func (s *StatsService) getDashboardKeywords(startTime string) ([]model.KeywordRankItem, error) {
	rows, err := s.db.Query(`
		SELECT keyword, COUNT(*) AS cnt
		FROM query_logs WHERE queried_at >= ?
		GROUP BY keyword ORDER BY cnt DESC LIMIT 10`, startTime)
	if err != nil {
		return nil, fmt.Errorf("查询搜索词排行失败: %w", err)
	}
	defer rows.Close()

	var items []model.KeywordRankItem
	for rows.Next() {
		var item model.KeywordRankItem
		if err := rows.Scan(&item.Keyword, &item.Count); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if items == nil {
		items = []model.KeywordRankItem{}
	}
	return items, nil
}

// getDashboardNodeDist 获取节次分布
func (s *StatsService) getDashboardNodeDist(startTime string) ([]model.NodeDistItem, error) {
	rows, err := s.db.Query(`
		SELECT start_node || '-' || end_node AS node_range, COUNT(*) AS cnt
		FROM query_logs
		WHERE queried_at >= ? AND start_node != '' AND end_node != ''
		GROUP BY node_range ORDER BY cnt DESC LIMIT 10`, startTime)
	if err != nil {
		return nil, fmt.Errorf("查询节次分布失败: %w", err)
	}
	defer rows.Close()

	var items []model.NodeDistItem
	for rows.Next() {
		var item model.NodeDistItem
		if err := rows.Scan(&item.Node, &item.Count); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if items == nil {
		items = []model.NodeDistItem{}
	}
	return items, nil
}

// getDashboardResultStats 获取查询结果统计
func (s *StatsService) getDashboardResultStats(startTime string) (model.ResultStatsData, error) {
	var r model.ResultStatsData

	// 基础统计
	err := s.db.QueryRow(`
		SELECT COALESCE(AVG(result_count), 0), COALESCE(MAX(result_count), 0),
		       COALESCE(MIN(CASE WHEN result_count > 0 THEN result_count END), 0),
		       COALESCE(SUM(CASE WHEN result_count = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN result_count > 0 THEN 1 ELSE 0 END), 0)
		FROM query_logs WHERE queried_at >= ?`, startTime,
	).Scan(&r.AvgCount, &r.MaxCount, &r.MinCount, &r.ZeroCount, &r.NonZeroCount)
	if err != nil {
		return r, fmt.Errorf("查询结果统计失败: %w", err)
	}

	// 区间分布
	type distRange struct {
		label string
		min   int
		max   int
	}
	ranges := []distRange{
		{"0", 0, 0},
		{"1-5", 1, 5},
		{"6-10", 6, 10},
		{"11-20", 11, 20},
		{"21-50", 21, 50},
		{"50+", 51, 999999},
	}

	for _, dr := range ranges {
		var count int
		err := s.db.QueryRow(`
			SELECT COUNT(*) FROM query_logs
			WHERE queried_at >= ? AND result_count >= ? AND result_count <= ?`,
			startTime, dr.min, dr.max,
		).Scan(&count)
		if err != nil {
			return r, fmt.Errorf("查询结果区间分布失败: %w", err)
		}
		r.Distribution = append(r.Distribution, model.ResultDistItem{Range: dr.label, Count: count})
	}

	return r, nil
}

// getDashboardHourlyDist 获取每小时查询分布
func (s *StatsService) getDashboardHourlyDist(startTime string) ([]model.HourlyDistItem, error) {
	rows, err := s.db.Query(`
		SELECT CAST(strftime('%H', queried_at) AS INTEGER) AS hour, COUNT(*) AS cnt
		FROM query_logs WHERE queried_at >= ?
		GROUP BY hour ORDER BY hour`, startTime)
	if err != nil {
		return nil, fmt.Errorf("查询每小时分布失败: %w", err)
	}
	defer rows.Close()

	hourMap := make(map[int]int)
	for rows.Next() {
		var hour, count int
		if err := rows.Scan(&hour, &count); err != nil {
			return nil, err
		}
		hourMap[hour] = count
	}

	// 填充 0-23 小时
	items := make([]model.HourlyDistItem, 24)
	for h := 0; h < 24; h++ {
		items[h] = model.HourlyDistItem{Hour: h, Count: hourMap[h]}
	}
	return items, nil
}

// DB 返回底层数据库连接，供其他服务复用
func (s *StatsService) DB() *sql.DB {
	return s.db
}

// Close 关闭数据库连接
func (s *StatsService) Close() error {
	return s.db.Close()
}
