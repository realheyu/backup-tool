package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupDirectoryUsesUTCPlus8(t *testing.T) {
	instant := time.Date(2026, 11, 5, 20, 4, 5, 0, time.UTC)
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("UTC-7", -7*60*60)} {
		t.Run(zone.String(), func(t *testing.T) {
			c := fixture(t)
			if err := runBackupAt(context.Background(), c, nil, instant.In(zone)); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.root(), "2026-11-06_04-04-05", "backup.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("backup directory does not use Beijing time", err)
			}
			var info BackupInfo
			if err := json.Unmarshal(data, &info); err != nil {
				t.Fatal(err)
			}
			if info.ID != "2026-11-06_04-04-05" || info.CreatedAt.Format(time.RFC3339) != "2026-11-06T04:04:05+08:00" {
				t.Fatal("incorrect backup timestamp", info.ID, info.CreatedAt)
			}
		})
	}
}

func TestSameSecondBackupNeverOverwritesExistingRun(t *testing.T) {
	c := fixture(t)
	start := time.Now().Truncate(time.Second)
	log := filepath.Join(t.TempDir(), "dump-args")
	t.Setenv("FAKE_ARGS", log)
	if err := runBackupAt(context.Background(), c, nil, start); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(c.root(), backupID(start), "backup.json")
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	beforeArgs, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := runBackupAt(context.Background(), c, nil, start.Add(900*time.Millisecond)); err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatal("same-second backup did not reject existing directory", err)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing backup was modified", err)
	}
	afterArgs, err := os.ReadFile(log)
	if err != nil || !bytes.Equal(beforeArgs, afterArgs) {
		t.Fatal("collision invoked mysqldump", err)
	}
}

func TestBackupDirectoryCollisionPreservesEmptyAndPartialDirectories(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "completed"
		if partial {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			at := time.Now()
			dir := filepath.Join(c.root(), backupID(at))
			if partial {
				dir += ".partial"
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if partial {
				if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("existing partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runBackupAt(context.Background(), c, nil, at); err == nil {
				t.Fatal("existing directory accepted")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || (partial && len(entries) != 1) || (!partial && len(entries) != 0) {
				t.Fatal("pre-existing directory changed", entries, err)
			}
		})
	}
}

func TestPruneUsesCorrectTimezoneForNewAndLegacyNames(t *testing.T) {
	c := fixture(t)
	if err := ensureDir(c.root()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Duration(c.KeepDays) * 24 * time.Hour)
	for _, partial := range []string{"", ".partial"} {
		for _, format := range []struct {
			name string
			make func(time.Time) string
		}{
			{"new", backupID},
			{"legacy", func(at time.Time) string { return at.UTC().Format("20060102T150405Z") + "-0123456789abcdef" }},
		} {
			old := filepath.Join(c.root(), format.make(cutoff.Add(-time.Second))+partial)
			boundary := filepath.Join(c.root(), format.make(cutoff)+partial)
			for _, path := range []string{old, boundary} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := prune(c, now); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatal("expired directory retained", format.name, partial)
			}
			if _, err := os.Stat(boundary); err != nil {
				t.Fatal("retention boundary deleted due to timezone", format.name, partial, err)
			}
			if err := os.Remove(boundary); err != nil {
				t.Fatal(err)
			}
		}
	}
}
