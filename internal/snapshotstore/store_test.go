package snapshotstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishSwitchesCurrentAndKeepsPrevious(t *testing.T) {
	root := t.TempDir()
	first := writeSource(t, "first")
	second := writeSource(t, "second")
	third := writeSource(t, "third")

	if err := Publish(root, "rel-1", first); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, root, "rel-1", "first")

	if err := Publish(root, "rel-2", second); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, root, "rel-2", "second")
	if _, err := os.Stat(releaseDir(root, "rel-1")); err != nil {
		t.Fatalf("上一版应保留: %v", err)
	}

	if err := Publish(root, "rel-3", third); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, root, "rel-3", "third")
	if _, err := os.Stat(releaseDir(root, "rel-1")); !os.IsNotExist(err) {
		t.Fatalf("更早的 release 应删除，stat err=%v", err)
	}
	if _, err := os.Stat(releaseDir(root, "rel-2")); err != nil {
		t.Fatalf("上一版应保留: %v", err)
	}
}

func TestPublishRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	source := writeSource(t, "ok")
	if err := Publish(root, "../escape", source); err == nil {
		t.Fatal("含路径分隔符的 releaseID 应被拒绝")
	}
	if err := Publish(root, "rel-1", t.TempDir()); err == nil {
		t.Fatal("缺少 manifest 的目录应被拒绝")
	}
	if err := Publish(root, "rel-1", source); err != nil {
		t.Fatal(err)
	}
	if err := Publish(root, "rel-1", source); err == nil {
		t.Fatal("重复 releaseID 应被拒绝")
	}
}

func TestCurrentLinkIsRelative(t *testing.T) {
	root := t.TempDir()
	if err := Publish(root, "rel-1", writeSource(t, "ok")); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, currentLinkName))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) {
		t.Fatalf("current 必须是相对链接，实际 %s", target)
	}
}

func writeSource(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertCurrent(t *testing.T, root, releaseID, marker string) {
	t.Helper()
	dataDir, err := CurrentDataDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != marker {
		t.Fatalf("当前内容=%q，想要 %q", got, marker)
	}
	if filepath.Base(filepath.Dir(dataDir)) != releaseID {
		t.Fatalf("当前 release=%s，想要 %s", filepath.Base(filepath.Dir(dataDir)), releaseID)
	}
}
