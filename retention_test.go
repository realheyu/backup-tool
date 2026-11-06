package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneRemovesExpiredIncompleteAndCorruptBackups(t *testing.T) {
	for _, state := range []string{"missing-manifest", "corrupt-manifest", "corrupt-payload", "partial"} {
		t.Run(state, func(t *testing.T) {
			c := fixture(t)
			if err := ensureDir(c.root()); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
			cutoff := now.Add(-time.Duration(c.KeepDays) * 24 * time.Hour)
			var expired, boundary string
			for _, at := range []time.Time{cutoff.Add(-time.Second), cutoff} {
				name := backupID(at)
				if state == "partial" {
					name += ".partial"
				}
				dir := filepath.Join(c.root(), name)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if state != "missing-manifest" {
					manifest := `{"format":1}`
					if state == "corrupt-manifest" {
						manifest = "broken JSON"
					}
					if err := os.WriteFile(filepath.Join(dir, "backup.json"), []byte(manifest), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "001-demo.sql.gz"), []byte("incomplete gzip"), 0600); err != nil {
					t.Fatal(err)
				}
				if at.Before(cutoff) {
					expired = dir
				} else {
					boundary = dir
				}
			}
			if err := prune(c, now); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(expired); !os.IsNotExist(err) {
				t.Fatal("expired incomplete/corrupt data retained")
			}
			if _, err := os.Stat(boundary); err != nil {
				t.Fatal("backup at retention boundary removed", err)
			}
		})
	}
}

func TestBackupAndCleanupErrorsAreBothReported(t *testing.T) {
	c := fixture(t)
	if err := runBackup(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	old := ageRun(t, runs(t, c)[0], time.Now().Add(-30*24*time.Hour))
	// This cannot be produced by the tool, but exercises continuing after a
	// cleanup error without relying on platform-specific directory permissions.
	invalid := filepath.Join(c.root(), "20009999T000000Z-0123456789abcdef")
	if err := os.Mkdir(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FAIL", "1")
	err := runBackup(context.Background(), c, nil)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || !strings.Contains(err.Error(), "清理本机过期备份") {
		t.Fatal("backup or cleanup error lost", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("one cleanup error prevented pruning other expired backups")
	}
}

func TestRetentionCannotBeDisabled(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte("keep_days="+value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p); err == nil {
			t.Fatal("accepted disabled retention", value)
		}
	}
}
