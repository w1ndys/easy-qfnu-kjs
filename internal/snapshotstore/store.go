package snapshotstore

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	releasesDirName = "releases"
	currentLinkName = "current"
	dataDirName     = "data"
	manifestName    = "manifest.json"
	stagingLinkName = ".current.tmp"
)

// Publish 把 sourceDir（结构与 data/ 相同，根上有 manifest.json）写入本机快照库。
//
// 成功后的布局：
//
//	<root>/releases/<releaseID>/data/...
//	<root>/current -> releases/<releaseID>
//
// current 是相对符号链接。先写临时链接再 rename，同一文件系统上切换是原子的。
// 调用方必须把整个 root 挂进容器，不能把 current 本身当挂载点，否则容器会钉住旧目录。
//
// 只保留当前版和上一版。更早的 release 在切换成功后删除。
// sourceDir 不会被修改或删除。
func Publish(root, releaseID, sourceDir string) error {
	if err := checkReleaseID(releaseID); err != nil {
		return err
	}
	if err := checkSource(sourceDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, releasesDirName), 0o755); err != nil {
		return fmt.Errorf("创建 releases 目录失败: %w", err)
	}
	dest := releaseDir(root, releaseID)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("release %s 已存在，拒绝覆盖", releaseID)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查 release 目录失败: %w", err)
	}
	if err := copyAsData(sourceDir, dest); err != nil {
		return cleanupFailedRelease(dest, err)
	}
	previous, err := readCurrentRelease(root)
	if err != nil {
		return cleanupFailedRelease(dest, err)
	}
	if err := swapCurrent(root, releaseID); err != nil {
		return cleanupFailedRelease(dest, err)
	}
	return pruneOld(root, releaseID, previous)
}

// CurrentDataDir 返回当前生效快照的 data 目录（含 manifest.json）。
// 没有 current，或链接指向的 manifest 不存在时返回错误。
func CurrentDataDir(root string) (string, error) {
	releaseID, err := readCurrentRelease(root)
	if err != nil {
		return "", err
	}
	if releaseID == "" {
		return "", fmt.Errorf("还没有已发布快照")
	}
	dataDir := filepath.Join(releaseDir(root, releaseID), dataDirName)
	manifest := filepath.Join(dataDir, manifestName)
	if _, err := os.Stat(manifest); err != nil {
		return "", fmt.Errorf("当前快照缺少 manifest.json: %w", err)
	}
	return dataDir, nil
}

func checkReleaseID(releaseID string) error {
	if releaseID == "" || releaseID == "." || releaseID == ".." {
		return fmt.Errorf("releaseID 无效")
	}
	if strings.Contains(releaseID, "/") || strings.Contains(releaseID, "\\") {
		return fmt.Errorf("releaseID 不能包含路径分隔符")
	}
	return nil
}

func checkSource(sourceDir string) error {
	manifest := filepath.Join(sourceDir, manifestName)
	info, err := os.Stat(manifest)
	if err != nil {
		return fmt.Errorf("候选目录缺少 manifest.json: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("候选 manifest.json 不是文件")
	}
	return nil
}

func releaseDir(root, releaseID string) string {
	return filepath.Join(root, releasesDirName, releaseID)
}

func readCurrentRelease(root string) (string, error) {
	link := filepath.Join(root, currentLinkName)
	target, err := os.Readlink(link)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("读取 current 链接失败: %w", err)
	}
	cleaned := filepath.Clean(target)
	prefix := releasesDirName + string(os.PathSeparator)
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("current 不是指向 releases/ 的相对链接: %s", target)
	}
	releaseID := strings.TrimPrefix(cleaned, prefix)
	if err := checkReleaseID(releaseID); err != nil {
		return "", err
	}
	return releaseID, nil
}

func swapCurrent(root, releaseID string) error {
	tmp := filepath.Join(root, stagingLinkName)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理临时链接失败: %w", err)
	}
	rel := filepath.Join(releasesDirName, releaseID)
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("创建临时链接失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(root, currentLinkName)); err != nil {
		return fmt.Errorf("切换 current 失败: %w", err)
	}
	return nil
}

func cleanupFailedRelease(dest string, cause error) error {
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("%w；清理未完成的 release 也失败: %v", cause, err)
	}
	return cause
}

func pruneOld(root, currentID, previousID string) error {
	entries, err := os.ReadDir(filepath.Join(root, releasesDirName))
	if err != nil {
		return fmt.Errorf("读取 releases 失败: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == currentID || name == previousID {
			continue
		}
		if err := os.RemoveAll(releaseDir(root, name)); err != nil {
			return fmt.Errorf("删除旧 release %s 失败: %w", name, err)
		}
	}
	return nil
}

func copyAsData(sourceDir, releaseDir string) error {
	dataDir := filepath.Join(releaseDir, dataDirName)
	if err := copyTree(sourceDir, dataDir); err != nil {
		return fmt.Errorf("复制候选数据失败: %w", err)
	}
	return syncTree(dataDir)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	copyErr := writeCopiedFile(dst, in)
	if cerr := in.Close(); cerr != nil {
		if copyErr == nil {
			return fmt.Errorf("关闭源文件失败: %w", cerr)
		}
		return fmt.Errorf("%w；关闭源文件也失败: %v", copyErr, cerr)
	}
	return copyErr
}

func writeCopiedFile(dst string, in *os.File) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return closeWithCause(out, err)
	}
	if err := out.Sync(); err != nil {
		return closeWithCause(out, err)
	}
	return out.Close()
}

func closeWithCause(file *os.File, cause error) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w；关闭文件也失败: %v", cause, err)
	}
	return cause
}

func syncTree(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}
