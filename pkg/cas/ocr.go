package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

// OCR 识别仅支持本地命令模式（无网络识别服务）：
// 环境变量 OCR_CMD 指向可执行命令（可带参数，空格分隔）；默认使用仓库内
// scripts/ocr_ddddocr.py。验证码图片字节写入临时文件并把路径作为命令的
// 最后一个参数传入，同时把图片字节通过 stdin 管道提供给命令；命令 stdout
// 的首行（去除首尾空白）即为识别结果。
const (
	defaultOCRCommand = "scripts/ocr_ddddocr.py"
	ocrRunTimeout     = 30 * time.Second
)

// resolveOCRCommand 解析 OCR 命令（OCR_CMD 或默认脚本），并检查其可执行。
// 返回拆分后的 argv。
func resolveOCRCommand() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("OCR_CMD"))
	if raw == "" {
		raw = defaultOCRCommand
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil, errors.New("OCR_CMD 为空")
	}
	exe := fields[0]
	if strings.ContainsRune(exe, os.PathSeparator) || strings.HasPrefix(exe, ".") {
		if _, err := os.Stat(exe); err != nil {
			return nil, fmt.Errorf("OCR 命令不存在：%s（设置 OCR_CMD 指向可执行脚本或命令）", exe)
		}
	} else if _, err := exec.LookPath(exe); err != nil {
		return nil, fmt.Errorf("OCR 命令不在 PATH 中：%s（设置 OCR_CMD）", exe)
	}
	return fields, nil
}

// RecognizeCaptcha 用本地 OCR 命令识别验证码图片字节，返回识别文本。
// 命令不存在/执行失败/stdout 为空都会返回明确错误（由登录循环计入重试）。
func RecognizeCaptcha(ctx context.Context, image []byte) (string, error) {
	if len(image) == 0 {
		return "", errors.New("验证码图片为空")
	}
	argv, err := resolveOCRCommand()
	if err != nil {
		return "", err
	}

	// 图片写入临时文件（作为最后一个参数）；同时经 stdin 提供给命令。
	tmp, err := os.CreateTemp("", "easy-qfnu-kjs-captcha-*.png")
	if err != nil {
		return "", fmt.Errorf("创建验证码临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(image); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("写入验证码临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("关闭验证码临时文件失败: %w", err)
	}
	defer os.Remove(tmpPath)

	runCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, ocrRunTimeout)
		defer cancel()
	}

	fullArgs := append(append([]string{}, argv...), tmpPath)
	cmd := exec.CommandContext(runCtx, fullArgs[0], fullArgs[1:]...)
	cmd.Stdin = bytes.NewReader(image)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return "", fmt.Errorf("OCR 命令执行失败（%s）: %s", filepath.Base(fullArgs[0]), msg)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return "", fmt.Errorf("OCR 命令（%s）输出为空，无法识别验证码", filepath.Base(fullArgs[0]))
	}
	return text, nil
}
