package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runNotificationCLI(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	extra, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "BACKUP_TOOL_TEST_CLI=1", "BACKUP_TOOL_TEST_CONFIG="+path, "BACKUP_TOOL_TEST_ARGS="+string(extra))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCLIAllErrorsAttemptNotifications(t *testing.T) {
	for _, scenario := range []string{"config", "check-config", "password", "missing-dump", "cleanup", "lock", "weekly-state", "argument", "flag"} {
		t.Run(scenario, func(t *testing.T) {
			c := notificationFixture(t)
			log := filepath.Join(t.TempDir(), "notifications")
			t.Setenv("FAKE_NOTIFY_LOG", log)
			config := fmt.Sprintf("name=%q\nbackup_dir=%q\n[mysql]\ndump_binary=%q\ndatabases=['demo']\n[dingtalk]\nwebhook=%q\nsecret=%q\n", c.Name, c.BackupDir, c.MySQL.DumpBinary, c.DingTalk.Webhook, c.DingTalk.Secret)
			var args []string
			switch scenario {
			case "config", "check-config":
				config += "unknown_setting=true\n"
				if scenario == "check-config" {
					args = []string{"--check"}
				}
			case "password":
				const variable = "BACKUP_TOOL_TEST_UNSET_PASSWORD"
				t.Setenv(variable, "")
				if err := os.Unsetenv(variable); err != nil {
					t.Fatal(err)
				}
				config = strings.Replace(config, "[mysql]\n", "[mysql]\npassword_env='"+variable+"'\n", 1)
			case "missing-dump":
				config = strings.Replace(config, c.MySQL.DumpBinary, filepath.Join(t.TempDir(), "does-not-exist"), 1)
			case "cleanup", "lock", "weekly-state":
				if err := ensureDir(c.root()); err != nil {
					t.Fatal(err)
				}
				if scenario == "cleanup" {
					if err := os.Mkdir(filepath.Join(c.root(), "20009999T000000Z-0123456789abcdef"), 0700); err != nil {
						t.Fatal(err)
					}
				} else if scenario == "weekly-state" {
					if err := os.WriteFile(filepath.Join(c.root(), "notification-state.json"), []byte("broken JSON"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					lock, err := os.OpenFile(filepath.Join(c.root(), ".lock"), os.O_CREATE|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
					if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				}
			case "argument":
				args = []string{"unsupported-subcommand"}
			case "flag":
				args = []string{"--unknown-flag"}
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := runNotificationCLI(t, path, args...)
			if err == nil {
				t.Fatal("failure returned success", out)
			}
			data, err := os.ReadFile(log)
			if err != nil || string(data) != "sent\n" {
				t.Fatalf("wanted one error notification: %q (%v), output: %s", data, err, out)
			}
		})
	}
}

func TestCLIWeeklyDeliveryFailureAlertsAndRetries(t *testing.T) {
	c := notificationFixture(t)
	if err := ensureDir(c.root()); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-weeklyInterval - time.Hour)
	statePath := filepath.Join(c.root(), "notification-state.json")
	if err := saveNotificationState(statePath, notificationState{Format: 1, Since: since, Succeeded: 6}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	config := fmt.Sprintf("name=%q\nbackup_dir=%q\n[mysql]\ndump_binary=%q\ndatabases=['demo']\n[dingtalk]\nwebhook=%q\nsecret=%q\n", c.Name, c.BackupDir, c.MySQL.DumpBinary, c.DingTalk.Webhook, c.DingTalk.Secret)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "notifications")
	t.Setenv("FAKE_NOTIFY_LOG", log)
	t.Setenv("BACKUP_TOOL_TEST_FAIL_WEEKLY", "1")
	if out, err := runNotificationCLI(t, path); err == nil || !strings.Contains(out, "发送每周汇总") {
		t.Fatal("summary failure not returned", out, err)
	}
	ageRun(t, runs(t, c)[0], time.Now().Add(-time.Hour))
	t.Setenv("BACKUP_TOOL_TEST_FAIL_WEEKLY", "0")
	if out, err := runNotificationCLI(t, path); err != nil {
		t.Fatal(out, err)
	}
	current := runs(t, c)
	ageRun(t, current[len(current)-1], time.Now().Add(-2*time.Hour))
	if out, err := runNotificationCLI(t, path); err != nil {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(log)
	if err != nil || string(data) != "weekly-failed\nsent\nweekly\n" {
		t.Fatal("missing failure alert, retry, or unwanted success notification", string(data), err)
	}
}
