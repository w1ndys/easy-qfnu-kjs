package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

var (
	schemaMu       sync.Mutex
	schemaCompiled = map[string]*jsonschema.Schema{}
)

// compileSchema 编译指定目录下的 schema 文件（按文件内 $id 注册，缓存结果）。
func compileSchema(schemaDir, name string) (*jsonschema.Schema, error) {
	key := filepath.Join(schemaDir, name)
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if s, ok := schemaCompiled[key]; ok {
		return s, nil
	}
	raw, err := os.ReadFile(key)
	if err != nil {
		return nil, err
	}
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.ID == "" {
		return nil, fmt.Errorf("schema %s 缺少 $id", name)
	}
	compiler := jsonschema.NewCompiler()
	// 由采集器生成的时间字符串均符合 RFC3339；开启 format 断言以同时约束外部旧数据。
	compiler.AssertFormat = true
	if err := compiler.AddResource(meta.ID, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("注册 schema %s 失败: %w", name, err)
	}
	s, err := compiler.Compile(meta.ID)
	if err != nil {
		return nil, err
	}
	schemaCompiled[key] = s
	return s, nil
}

// ValidateSchemaFile 用指定 schema 校验文档字节，返回格式化后的错误。
func ValidateSchemaFile(schemaDir, schemaName string, doc []byte) error {
	s, err := compileSchema(schemaDir, schemaName)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return fmt.Errorf("文档不是合法 JSON: %w", err)
	}
	if err := s.Validate(v); err != nil {
		return fmt.Errorf("%s 校验失败: %w", schemaName, err)
	}
	return nil
}
