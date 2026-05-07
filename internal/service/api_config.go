package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

const defaultAIParsePrompt = `你是曲阜师范大学空教室查询参数解析器。请只返回 JSON，不要返回 Markdown。字段必须为 building、date_offset、start_node、end_node、confidence、reason。building 是教学楼名称，date_offset 是目标日期相对今天的整数偏移，今天为 0，明天为 1，后天为 2；start_node 和 end_node 是两位字符串 01 到 11。confidence 只能是 high 或 low。若描述缺少教学楼、日期或节次范围，confidence 必须为 low，并在 reason 中说明缺少什么。`

// APIConfigService 管理 AI 解析与开放 API 配置。
type APIConfigService struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewAPIConfigService(db *sql.DB) (*APIConfigService, error) {
	if err := migrateAPIConfigSchema(db); err != nil {
		return nil, fmt.Errorf("开放接口配置表迁移失败: %w", err)
	}
	logger.Info("开放接口配置服务已初始化")
	return &APIConfigService{db: db}, nil
}

func migrateAPIConfigSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS api_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			ai_base_url TEXT NOT NULL DEFAULT '',
			ai_key TEXT NOT NULL DEFAULT '',
			ai_model TEXT NOT NULL DEFAULT '',
			ai_prompt_override TEXT NOT NULL DEFAULT '',
			open_api_enabled INTEGER NOT NULL DEFAULT 0,
			open_api_key TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT (datetime('now', 'localtime'))
		);
		INSERT OR IGNORE INTO api_config (id) VALUES (1);
	`)
	if err != nil {
		return fmt.Errorf("创建 api_config 表失败: %w", err)
	}
	if err := ensureAPIConfigColumn(db, "ai_prompt_override", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func ensureAPIConfigColumn(db *sql.DB, name, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(api_config)`)
	if err != nil {
		return fmt.Errorf("读取 api_config 表结构失败: %w", err)
	}

	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("解析 api_config 表结构失败: %w", err)
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("遍历 api_config 表结构失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 api_config 表结构查询失败: %w", err)
	}

	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE api_config ADD COLUMN %s %s", name, definition)); err != nil {
		return fmt.Errorf("迁移 api_config.%s 字段失败: %w", name, err)
	}
	return nil
}

func (s *APIConfigService) Get() (*model.APIConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked()
}

func (s *APIConfigService) getLocked() (*model.APIConfig, error) {
	var cfg model.APIConfig
	var enabled int
	var promptOverride string
	err := s.db.QueryRow(`
		SELECT ai_base_url, ai_key, ai_model, ai_prompt_override, open_api_enabled, open_api_key
		FROM api_config WHERE id = 1
	`).Scan(&cfg.AIBaseURL, &cfg.AIKey, &cfg.AIModel, &promptOverride, &enabled, &cfg.OpenAPIKey)
	if err != nil {
		return nil, fmt.Errorf("读取开放接口配置失败: %w", err)
	}
	cfg.DefaultAIPrompt = defaultAIParsePrompt
	cfg.AIPrompt = defaultAIParsePrompt
	if promptOverride != "" {
		cfg.AIPrompt = promptOverride
		cfg.AIPromptOverridden = true
	}
	cfg.OpenAPIEnabled = enabled != 0
	return &cfg, nil
}

func (s *APIConfigService) Update(cfg model.APIConfig) (*model.APIConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg.AIBaseURL = strings.TrimSpace(cfg.AIBaseURL)
	cfg.AIKey = strings.TrimSpace(cfg.AIKey)
	cfg.AIModel = strings.TrimSpace(cfg.AIModel)
	cfg.AIPrompt = strings.TrimSpace(cfg.AIPrompt)
	cfg.OpenAPIKey = strings.TrimSpace(cfg.OpenAPIKey)
	promptOverride := cfg.AIPrompt
	if promptOverride == "" || promptOverride == defaultAIParsePrompt {
		promptOverride = ""
	}
	enabled := 0
	if cfg.OpenAPIEnabled {
		enabled = 1
	}

	_, err := s.db.Exec(`
		UPDATE api_config
		SET ai_base_url = ?, ai_key = ?, ai_model = ?, ai_prompt_override = ?, open_api_enabled = ?, open_api_key = ?, updated_at = datetime('now', 'localtime')
		WHERE id = 1
	`, cfg.AIBaseURL, cfg.AIKey, cfg.AIModel, promptOverride, enabled, cfg.OpenAPIKey)
	if err != nil {
		return nil, fmt.Errorf("保存开放接口配置失败: %w", err)
	}

	return s.getLocked()
}

func (s *APIConfigService) ResetAIPrompt() (*model.APIConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE api_config
		SET ai_prompt_override = '', updated_at = datetime('now', 'localtime')
		WHERE id = 1
	`)
	if err != nil {
		return nil, fmt.Errorf("恢复默认 AI 提示词失败: %w", err)
	}

	return s.getLocked()
}

func (s *APIConfigService) ValidateOpenAPIKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	cfg, err := s.Get()
	if err != nil || !cfg.OpenAPIEnabled || cfg.OpenAPIKey == "" {
		return false
	}
	return key == cfg.OpenAPIKey
}

func (s *APIConfigService) ListModels(ctx context.Context) ([]string, error) {
	cfg, err := s.Get()
	if err != nil {
		return nil, err
	}
	if cfg.AIBaseURL == "" || cfg.AIKey == "" {
		return nil, fmt.Errorf("请先配置 AI BaseURL 和 Key")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAIURL(cfg.AIBaseURL, "/models"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AIKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("获取模型列表失败，状态码: %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}

	models := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	return models, nil
}

func (s *APIConfigService) ParseNaturalLanguage(ctx context.Context, text string) (*model.AIParsedQuery, error) {
	cfg, err := s.Get()
	if err != nil {
		return nil, err
	}
	if cfg.AIBaseURL == "" || cfg.AIKey == "" || cfg.AIModel == "" {
		return nil, fmt.Errorf("AI 配置不完整，请先配置 BaseURL、Key 和 Model")
	}

	payload := map[string]any{
		"model": cfg.AIModel,
		"messages": []map[string]string{
			{"role": "system", "content": cfg.AIPrompt},
			{"role": "user", "content": strings.TrimSpace(text)},
		},
		"temperature": 0,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIURL(cfg.AIBaseURL, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AI 解析请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI 解析失败，状态码: %d", resp.StatusCode)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &completion); err != nil {
		return nil, fmt.Errorf("解析 AI 响应失败: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("AI 未返回解析结果")
	}

	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end > start {
			content = content[start : end+1]
		}
	}

	var parsed model.AIParsedQuery
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("AI 返回内容不是有效 JSON")
	}
	parsed.BuildingName = strings.TrimSpace(parsed.BuildingName)
	parsed.StartNode = normalizeNode(parsed.StartNode)
	parsed.EndNode = normalizeNode(parsed.EndNode)
	parsed.Confidence = strings.ToLower(strings.TrimSpace(parsed.Confidence))
	if parsed.Confidence == "" {
		parsed.Confidence = "low"
	}
	return &parsed, nil
}

func openAIURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + path
	}
	return baseURL + "/v1" + path
}

func normalizeNode(node string) string {
	node = strings.TrimSpace(node)
	node = regexp.MustCompile(`\D`).ReplaceAllString(node, "")
	if len(node) == 1 && node >= "1" && node <= "9" {
		return "0" + node
	}
	return node
}
