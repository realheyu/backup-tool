package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type FileInfo struct {
	Name      string   `json:"name"`
	Databases []string `json:"databases"`
	Size      int64    `json:"size"`
	SHA256    string   `json:"sha256"`
}
type BackupInfo struct {
	Format    int        `json:"format"`
	Name      string     `json:"name"`
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	Files     []FileInfo `json:"files"`
}

const backupTimeLayout = "2006-01-02_15-04-05"

var backupZone = time.FixedZone("UTC+8", 8*60*60)
var runID = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{2}-[0-9]{2}-[0-9]{2}$`)
var legacyRunID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)
var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func backupID(at time.Time) string { return at.In(backupZone).Format(backupTimeLayout) }

func backupCreatedAt(id string) (time.Time, error) {
	if runID.MatchString(id) {
		return time.ParseInLocation(backupTimeLayout, id, backupZone)
	}
	stamp, _, _ := strings.Cut(id, "-")
	return time.Parse("20060102T150405Z", stamp)
}

func requireNewBackupDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("备份目录已存在，拒绝覆盖，请下一秒重试: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func ensureDir(path string) error {
	if e := os.MkdirAll(path, 0700); e != nil {
		return e
	}
	s, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("目录不得为符号链接: %s", path)
	}
	return nil
}
func runBackup(ctx context.Context, c Config, password *string) error {
	return runBackupAt(ctx, c, password, time.Now())
}

func runBackupAt(ctx context.Context, c Config, password *string, startedAt time.Time) (err error) {
	if len(c.MySQL.Databases) == 0 {
		return fmt.Errorf("mysql.databases 必须明确填写要备份的数据库，不能留空")
	}
	if e := ensureDir(c.root()); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(c.root(), ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("同一备份目录已有任务运行: %w", e)
	}
	// Retention is independent of export/sync success. Keep the lock until
	// pruning finishes, and preserve both the original and cleanup errors.
	defer func() {
		if e := prune(c, time.Now()); e != nil {
			err = errors.Join(err, fmt.Errorf("清理本机过期备份: %w", e))
		}
	}()
	id := backupID(startedAt)
	dir := filepath.Join(c.root(), id)
	if e = requireNewBackupDir(dir); e != nil {
		return e
	}
	info := BackupInfo{Format: 1, Name: c.Name, ID: id, CreatedAt: startedAt.In(backupZone)}
	partial := filepath.Join(c.root(), id+".partial")
	if e = os.Mkdir(partial, 0700); e != nil {
		return fmt.Errorf("创建本次临时备份目录失败（不会覆盖已有目录）: %w", e)
	}
	defer func() {
		if e := os.RemoveAll(partial); e != nil {
			err = errors.Join(err, fmt.Errorf("清理本次临时备份: %w", e))
		}
	}()
	for i, db := range c.MySQL.Databases {
		if e = ctx.Err(); e != nil {
			return e
		}
		label := unsafeFile.ReplaceAllString(db, "_")
		if len(label) > 64 {
			label = label[:64]
		}
		file := fmt.Sprintf("%03d-%s.sql.gz", i+1, label)
		fmt.Printf("备份 %s → %s\n", db, file)
		entry, e := createDump(ctx, c, password, db, filepath.Join(partial, file))
		if e != nil {
			return e
		}
		info.Files = append(info.Files, entry)
	}
	if e = writeJSON(filepath.Join(partial, "backup.json"), info); e != nil {
		return e
	}
	if e = requireNewBackupDir(dir); e != nil {
		return e
	}
	if e = os.Rename(partial, dir); e != nil {
		return e
	}
	fmt.Println("本地备份:", dir)
	if e = syncBackup(ctx, c, dir, info); e != nil {
		return fmt.Errorf("同步失败，本地备份按保留天数清理: %w", e)
	}
	// This marker records sync success; retention does not depend on it.
	if e = writeJSON(filepath.Join(dir, "synced.json"), struct {
		At time.Time `json:"at"`
	}{time.Now().In(backupZone)}); e != nil {
		return e
	}
	return nil
}

type countWriter struct {
	out io.Writer
	n   int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, e := w.out.Write(p)
	w.n += int64(n)
	return n, e
}
func createDump(ctx context.Context, c Config, password *string, db string, path string) (FileInfo, error) {
	entry := FileInfo{Name: filepath.Base(path), Databases: []string{db}}
	binary, args, cleanup, e := dumpCommand(c, password, db)
	if e != nil {
		return entry, e
	}
	defer cleanup()
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return entry, e
	}
	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, h))
	counter := &countWriter{out: gz}
	runErr := execute(ctx, c, binary, args, counter)
	gzErr := gz.Close()
	syncErr := f.Sync()
	closeErr := f.Close()
	if e = errors.Join(runErr, gzErr, syncErr, closeErr); e != nil {
		_ = os.Remove(path)
		return entry, fmt.Errorf("导出 %s: %w", db, e)
	}
	if counter.n == 0 {
		_ = os.Remove(path)
		return entry, fmt.Errorf("导出内容为空: %s", db)
	}
	s, e := os.Stat(path)
	if e != nil {
		return entry, e
	}
	entry.Size = s.Size()
	entry.SHA256 = hex.EncodeToString(h.Sum(nil))
	if e = verifyFile(path, entry); e != nil {
		return entry, e
	}
	return entry, nil
}
func verifyFile(path string, entry FileInfo) error {
	s, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !s.Mode().IsRegular() || s.Size() != entry.Size {
		return fmt.Errorf("备份文件类型或大小不正确: %s", path)
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return e
	}
	if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("SHA256 校验失败: %s", path)
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	gz, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	n, e := io.Copy(io.Discard, gz)
	ce := gz.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if n == 0 {
		return fmt.Errorf("备份解压后为空")
	}
	return nil
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path+".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, we := f.Write(append(b, '\n'))
	se := f.Sync()
	ce := f.Close()
	if e = errors.Join(we, se, ce); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func prune(c Config, now time.Time) error {
	if c.KeepDays <= 0 {
		return fmt.Errorf("keep_days 必须大于 0")
	}
	entries, e := os.ReadDir(c.root())
	if e != nil {
		return e
	}
	cutoff := now.Add(-time.Duration(c.KeepDays) * 24 * time.Hour)
	var errs []error
	for _, d := range entries {
		id := strings.TrimSuffix(d.Name(), ".partial")
		if !d.IsDir() || (!runID.MatchString(id) && !legacyRunID.MatchString(id)) {
			continue
		}
		// Keep recognizing old UTC/random names so existing backups still
		// expire, even if their manifests are missing or corrupt.
		createdAt, e := backupCreatedAt(id)
		if e != nil {
			errs = append(errs, fmt.Errorf("备份目录时间无效 %s: %w", d.Name(), e))
			continue
		}
		if !createdAt.Before(cutoff) {
			continue
		}
		dir := filepath.Join(c.root(), d.Name())
		fmt.Println("删除过期备份:", dir)
		if e = os.RemoveAll(dir); e != nil {
			errs = append(errs, fmt.Errorf("删除 %s: %w", dir, e))
		}
	}
	return errors.Join(errs...)
}
