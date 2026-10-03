// 本文件是 entry 层：执行数据库迁移的命令行入口。
// 用法：DATABASE_URL=postgres://... go run ./cmd/migrate

package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// main 读取部署级连接串，建连后应用尚未执行的迁移。
func main() {
	// 连接串来自环境变量，业务设置不在这里
	databaseURL := os.Getenv("DATABASE_URL")

	// 迁移是本机建库动作，给两分钟足够，超时即放弃而不是挂着
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	applied, err := s.Migrate(ctx)
	if err != nil {
		log.Fatalf("迁移失败: %v", err)
	}

	// 没有新迁移也明确输出，便于判断重复执行确实是幂等的
	if len(applied) == 0 {
		log.Println("没有待应用的迁移，数据库结构已是最新")
		return
	}
	log.Printf("已应用 %d 个迁移: %s", len(applied), strings.Join(applied, ", "))
}
