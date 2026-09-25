package collector

import (
	"context"
	"os"
	"path/filepath"
)

// readPublishedFile 读取已发布的 manifest 或周快照原文。
// 配置了数据库时先读当前 release；库里还没有当前版时，退回仓库 data/。
func readPublishedFile(opts Options, rel string) ([]byte, string, error) {
	if opts.DatabaseURL != "" {
		b, err := readPublishedFromDB(context.Background(), opts.DatabaseURL, rel)
		if err == nil {
			return b, "postgres", nil
		}
		if !os.IsNotExist(err) {
			return nil, "", err
		}
	}
	path := filepath.Join(opts.DataDir, rel)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return b, path, nil
}
