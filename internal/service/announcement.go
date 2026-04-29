package service

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

// AnnouncementService 公告管理服务
type AnnouncementService struct {
	db *sql.DB
	mu sync.Mutex
}

// NewAnnouncementService 创建公告服务，复用已有的 SQLite 数据库连接
func NewAnnouncementService(db *sql.DB) (*AnnouncementService, error) {
	if err := migrateAnnouncementSchema(db); err != nil {
		return nil, fmt.Errorf("公告表迁移失败: %w", err)
	}
	logger.Info("公告服务已初始化")
	return &AnnouncementService{db: db}, nil
}

// migrateAnnouncementSchema 创建公告表（如果不存在）
func migrateAnnouncementSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS announcements (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			important INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT (datetime('now', 'localtime')),
			updated_at DATETIME DEFAULT (datetime('now', 'localtime'))
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 announcements 表失败: %w", err)
	}
	return nil
}

// List 获取所有公告（按创建时间倒序）
func (s *AnnouncementService) List() ([]model.Announcement, error) {
	rows, err := s.db.Query(`
		SELECT id, title, content, important, created_at, updated_at
		FROM announcements ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("查询公告列表失败: %w", err)
	}
	defer rows.Close()

	var list []model.Announcement
	for rows.Next() {
		var a model.Announcement
		var imp int
		if err := rows.Scan(&a.ID, &a.Title, &a.Content, &imp, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描公告数据失败: %w", err)
		}
		a.Important = imp != 0
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历公告结果失败: %w", err)
	}
	if list == nil {
		list = []model.Announcement{}
	}
	return list, nil
}

// GetByID 根据 ID 获取单条公告
func (s *AnnouncementService) GetByID(id int64) (*model.Announcement, error) {
	var a model.Announcement
	var imp int
	err := s.db.QueryRow(`
		SELECT id, title, content, important, created_at, updated_at
		FROM announcements WHERE id = ?
	`, id).Scan(&a.ID, &a.Title, &a.Content, &imp, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询公告失败: %w", err)
	}
	a.Important = imp != 0
	return &a, nil
}

// Create 创建公告
func (s *AnnouncementService) Create(req model.CreateAnnouncementRequest) (*model.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	imp := 0
	if req.Important {
		imp = 1
	}

	result, err := s.db.Exec(`
		INSERT INTO announcements (title, content, important) VALUES (?, ?, ?)
	`, req.Title, req.Content, imp)
	if err != nil {
		return nil, fmt.Errorf("创建公告失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("获取公告 ID 失败: %w", err)
	}

	return s.GetByID(id)
}

// Update 更新公告
func (s *AnnouncementService) Update(id int64, req model.UpdateAnnouncementRequest) (*model.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	imp := 0
	if req.Important {
		imp = 1
	}

	result, err := s.db.Exec(`
		UPDATE announcements SET title = ?, content = ?, important = ?,
		updated_at = datetime('now', 'localtime') WHERE id = ?
	`, req.Title, req.Content, imp, id)
	if err != nil {
		return nil, fmt.Errorf("更新公告失败: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("获取影响行数失败: %w", err)
	}
	if affected == 0 {
		return nil, nil
	}

	return s.GetByID(id)
}

// Delete 删除公告
func (s *AnnouncementService) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM announcements WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("删除公告失败: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取影响行数失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("公告不存在")
	}
	return nil
}

// ListPublic 获取前台展示用的公告列表
func (s *AnnouncementService) ListPublic() ([]model.AnnouncementPublic, error) {
	announcements, err := s.List()
	if err != nil {
		return nil, err
	}

	var list []model.AnnouncementPublic
	for _, a := range announcements {
		// 提取日期部分 (created_at 格式: "2026-04-29 12:00:00")
		date := a.CreatedAt
		if len(date) >= 10 {
			date = date[:10]
		}
		list = append(list, model.AnnouncementPublic{
			ID:        fmt.Sprintf("announcement-%d", a.ID),
			Date:      date,
			Title:     a.Title,
			Content:   a.Content,
			Important: a.Important,
		})
	}
	if list == nil {
		list = []model.AnnouncementPublic{}
	}
	return list, nil
}
