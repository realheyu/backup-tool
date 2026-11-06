package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(p, []byte("#!/bin/sh\nset -eu\n"+body), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func fixture(t *testing.T) Config {
	t.Helper()
	dump := script(t, "mysqldump", `
if [ "${FAKE_ARGS:-}" != "" ]; then printf '%s\n' "$@" >> "$FAKE_ARGS"; fi
for arg in "$@"; do
 case "$arg" in
 --defaults-file=*)
  f=${arg#--defaults-file=}
  if [ "${FAKE_AUTH:-}" != "" ]; then cat "$f" > "$FAKE_AUTH"; fi
  if [ "${FAKE_TEMP:-}" != "" ]; then printf '%s' "$f" > "$FAKE_TEMP"; fi;;
 esac
done
if [ "${FAKE_EMPTY:-}" = 1 ]; then exit 0; fi
printf 'CREATE DATABASE demo;\nCREATE TABLE demo.t (id INT);\n'
if [ "${FAKE_FAIL:-}" = 1 ]; then printf 'authentication failed\n' >&2; exit 7; fi
`)
	return Config{Name: "test", BackupDir: t.TempDir(), KeepDays: 7, Timeout: "5s", MySQL: MySQL{DumpBinary: dump, SingleTransaction: true, Databases: []string{"demo", "payments"}}, Sync: Sync{Type: "none"}}
}
func runs(t *testing.T, c Config) []string {
	t.Helper()
	entries, e := os.ReadDir(c.root())
	if e != nil {
		t.Fatal(e)
	}
	var out []string
	for _, d := range entries {
		if d.IsDir() && runID.MatchString(d.Name()) {
			out = append(out, filepath.Join(c.root(), d.Name()))
		}
	}
	return out
}

func TestFailedAndEmptyDumpNeverBecomeBackup(t *testing.T) {
	for _, v := range []string{"FAKE_FAIL", "FAKE_EMPTY"} {
		t.Run(v, func(t *testing.T) {
			c := fixture(t)
			t.Setenv(v, "1")
			if e := runBackup(context.Background(), c, nil); e == nil {
				t.Fatal("expected failure")
			}
			if len(runs(t, c)) != 0 {
				t.Fatal("failed dump published")
			}
			entries, _ := os.ReadDir(c.root())
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".partial") {
					t.Fatal("partial directory retained after handled failure")
				}
			}
		})
	}
}
func TestEachDatabaseGetsItsOwnDump(t *testing.T) {
	for _, databases := range [][]string{{"demo"}, {"demo", "payments"}} {
		t.Run(strings.Join(databases, ","), func(t *testing.T) {
			c := fixture(t)
			c.MySQL.Databases = databases
			log := filepath.Join(t.TempDir(), "args")
			t.Setenv("FAKE_ARGS", log)
			if e := runBackup(context.Background(), c, nil); e != nil {
				t.Fatal(e)
			}
			r := runs(t, c)
			if len(r) != 1 {
				t.Fatal(r)
			}
			var info BackupInfo
			b, _ := os.ReadFile(filepath.Join(r[0], "backup.json"))
			if e := json.Unmarshal(b, &info); e != nil {
				t.Fatal(e)
			}
			if len(info.Files) != len(databases) {
				t.Fatal(info.Files)
			}
			for i, f := range info.Files {
				if len(f.Databases) != 1 || f.Databases[0] != databases[i] {
					t.Fatal("different databases combined in one dump", f)
				}
				if e := verifyFile(filepath.Join(r[0], f.Name), f); e != nil {
					t.Fatal(e)
				}
			}
			b, _ = os.ReadFile(log)
			for _, option := range []string{"--single-transaction", "--skip-lock-tables"} {
				if !strings.Contains(string(b), option+"\n") {
					t.Fatal("missing non-locking backup option", option)
				}
			}
			if strings.Contains(string(b), "--lock-all-tables") {
				t.Fatal("global table locking enabled")
			}
			if strings.Contains(string(b), "--all-databases") || strings.Count(string(b), "--databases\n") != len(databases) {
				t.Fatal("databases not exported by individual commands", string(b))
			}
			for _, db := range databases {
				if !strings.Contains(string(b), "--databases\n"+db+"\n") {
					t.Fatal("missing per-database command", db)
				}
			}
		})
	}
}
func TestPasswordTransportAndCleanup(t *testing.T) {
	for _, mode := range []string{"success", "FAKE_FAIL", "FAKE_EMPTY"} {
		t.Run(mode, func(t *testing.T) {
			c := fixture(t)
			c.MySQL.Databases = []string{"demo"}
			if mode != "success" {
				t.Setenv(mode, "1")
			}
			log := filepath.Join(t.TempDir(), "auth")
			tempLog := filepath.Join(t.TempDir(), "temp")
			argsLog := filepath.Join(t.TempDir(), "args")
			t.Setenv("FAKE_AUTH", log)
			t.Setenv("FAKE_TEMP", tempLog)
			t.Setenv("FAKE_ARGS", argsLog)
			password := "test-\\\" # ;$()\nsecond"
			if e := runBackup(context.Background(), c, &password); (e != nil) != (mode != "success") {
				t.Fatal("unexpected backup result", e)
			}
			b, e := os.ReadFile(log)
			if e != nil {
				t.Fatal(e)
			}
			expected, _ := passwordConfig(password)
			if string(b) != expected {
				t.Fatal("password option file mismatch")
			}
			b, _ = os.ReadFile(tempLog)
			if _, e = os.Stat(string(b)); !os.IsNotExist(e) {
				t.Fatal("temporary credentials remain", string(b))
			}
			b, _ = os.ReadFile(argsLog)
			if strings.Contains(string(b), password) {
				t.Fatal("password leaked in arguments")
			}
		})
	}
}
func TestSyncFailurePreservesCurrentAndPrunesExpiredBackups(t *testing.T) {
	c := fixture(t)
	if e := runBackup(context.Background(), c, nil); e != nil {
		t.Fatal(e)
	}
	old := ageRun(t, runs(t, c)[0], time.Now().Add(-30*24*time.Hour))
	c.Sync = ossFixture()
	requests := 0
	mockOSS(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodPut || strings.HasSuffix(r.URL.Path, "/backup.json") {
			t.Fatalf("unexpected request after failed upload: %s %s", r.Method, r.URL)
		}
		resp := response(`<Error><Code>AccessDenied</Code><Message>Denied</Message><RequestId>test</RequestId></Error>`)
		resp.StatusCode = http.StatusForbidden
		return resp, nil
	})
	if e := runBackup(context.Background(), c, nil); e == nil {
		t.Fatal("expected sync failure")
	}
	if _, e := os.Stat(old); !os.IsNotExist(e) {
		t.Fatal("expired backup retained after sync failure")
	}
	if len(runs(t, c)) != 1 {
		t.Fatal("local backup lost")
	}
	if requests != 1 {
		t.Fatalf("wanted one failed upload, got %d", requests)
	}
	failed := runs(t, c)[0]
	if _, e := os.Stat(filepath.Join(failed, "synced.json")); !os.IsNotExist(e) {
		t.Fatal("failed upload marked as synchronized")
	}
	// Repeated sync failures must also remove earlier failed uploads at expiry.
	failed = ageRun(t, failed, time.Now().Add(-30*24*time.Hour))
	if e := runBackup(context.Background(), c, nil); e == nil {
		t.Fatal("expected repeated sync failure")
	}
	if _, e := os.Stat(failed); !os.IsNotExist(e) {
		t.Fatal("expired failed upload retained")
	}
	if len(runs(t, c)) != 1 || requests != 2 {
		t.Fatal("unexpected retained backups or upload requests")
	}
}
func ageRun(t *testing.T, dir string, at time.Time) string {
	t.Helper()
	p := filepath.Join(dir, "backup.json")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var info BackupInfo
	if e = json.Unmarshal(b, &info); e != nil {
		t.Fatal(e)
	}
	info.CreatedAt = at.In(backupZone)
	info.ID = backupID(at)
	aged := filepath.Join(filepath.Dir(dir), info.ID)
	if e = os.Rename(dir, aged); e != nil {
		t.Fatal(e)
	}
	if e = writeJSON(filepath.Join(aged, "backup.json"), info); e != nil {
		t.Fatal(e)
	}
	return aged
}
func TestPruneOldOwnedBackupsRegardlessOfSync(t *testing.T) {
	c := fixture(t)
	c.KeepDays = 36500
	var aged []string
	for _, days := range []int{20, 21, 0} {
		if e := runBackup(context.Background(), c, nil); e != nil {
			t.Fatal(e)
		}
		currentRuns := runs(t, c)
		current := currentRuns[len(currentRuns)-1]
		// Move completed fixtures out of the current second before creating
		// another run, without relying on sleeps or wall-clock precision.
		if days > 0 {
			aged = append(aged, ageRun(t, current, time.Now().Add(-time.Duration(days)*24*time.Hour)))
		} else {
			aged = append(aged, current)
		}
	}
	old, pending, recent := aged[0], aged[1], aged[2]
	c.KeepDays = 7
	if e := os.Remove(filepath.Join(pending, "synced.json")); e != nil {
		t.Fatal(e)
	}
	unknown := filepath.Join(c.root(), "personal.sql.gz")
	if e := os.WriteFile(unknown, []byte("unrelated"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := prune(c, time.Now()); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{old, pending} {
		if _, e := os.Stat(p); !os.IsNotExist(e) {
			t.Fatal("expired backup retained", p)
		}
	}
	for _, p := range []string{recent, unknown} {
		if _, e := os.Stat(p); e != nil {
			t.Fatal("unrelated/recent removed", p)
		}
	}
}
func TestPruneDoesNotFollowSymlinks(t *testing.T) {
	c := fixture(t)
	if e := runBackup(context.Background(), c, nil); e != nil {
		t.Fatal(e)
	}
	r := ageRun(t, runs(t, c)[0], time.Now().Add(-30*24*time.Hour))
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if e := os.WriteFile(keep, []byte("unrelated"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(r, "unexpected")); e != nil {
		t.Fatal(e)
	}
	rootLink := filepath.Join(c.root(), "20000101T000000Z-0123456789abcdef")
	if e := os.Symlink(outside, rootLink); e != nil {
		t.Fatal(e)
	}
	if e := prune(c, time.Now()); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(r); !os.IsNotExist(e) {
		t.Fatal("expired directory retained")
	}
	for _, p := range []string{keep, rootLink} {
		if _, e := os.Stat(p); e != nil {
			t.Fatal("symlink target or root symlink removed", p, e)
		}
	}
}
func TestTimeoutKillsLocalProcessGroup(t *testing.T) {
	c := fixture(t)
	c.Timeout = "50ms"
	p := script(t, "hang", "sleep 10\n")
	start := time.Now()
	if e := execute(context.Background(), c, p, nil, io.Discard); e == nil {
		t.Fatal("missing timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout did not stop children")
	}
}
func TestCorruptionDetected(t *testing.T) {
	c := fixture(t)
	if e := runBackup(context.Background(), c, nil); e != nil {
		t.Fatal(e)
	}
	r := runs(t, c)[0]
	b, _ := os.ReadFile(filepath.Join(r, "backup.json"))
	var info BackupInfo
	json.Unmarshal(b, &info)
	p := filepath.Join(r, info.Files[0].Name)
	f, e := os.OpenFile(p, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteAt([]byte("broken"), 0)
	f.Close()
	if e != nil {
		t.Fatal(e)
	}
	if e = verifyFile(p, info.Files[0]); e == nil {
		t.Fatal("corruption accepted")
	}
}

func TestCLIHelper(t *testing.T) {
	if os.Getenv("BACKUP_TOOL_TEST_CLI") != "1" {
		return
	}
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && r.URL.Host == "example.oss.example.invalid" {
			resp := response(`<Error><Code>AccessDenied</Code><Message>test sync failure</Message><RequestId>test</RequestId></Error>`)
			resp.StatusCode = http.StatusForbidden
			resp.Status = "403 Forbidden"
			resp.Request = r
			return resp, nil
		}
		b, _ := io.ReadAll(r.Body)
		weekly := strings.Contains(string(b), "backup-tool 每周汇总")
		if r.URL.Query().Get("sign") == "" || (!weekly && !strings.Contains(string(b), "backup-tool 失败")) {
			return nil, fmt.Errorf("invalid notification request")
		}
		f, e := os.OpenFile(os.Getenv("FAKE_NOTIFY_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		marker := "sent"
		if weekly {
			marker = "weekly"
			if os.Getenv("BACKUP_TOOL_TEST_FAIL_WEEKLY") == "1" {
				marker = "weekly-failed"
			}
		}
		fmt.Fprintln(f, marker)
		f.Close()
		if marker == "weekly-failed" {
			return response(`{"errcode":310000,"errmsg":"test weekly failure"}`), nil
		}
		return response(`{"errcode":0,"errmsg":"ok"}`), nil
	})
	os.Args = []string{os.Args[0], "--config", os.Getenv("BACKUP_TOOL_TEST_CONFIG")}
	if extra := os.Getenv("BACKUP_TOOL_TEST_ARGS"); extra != "" {
		var args []string
		if err := json.Unmarshal([]byte(extra), &args); err != nil {
			t.Fatal(err)
		}
		os.Args = append(os.Args, args...)
	}
	os.Exit(cli())
}
func TestCLIOnlyNotifiesFailure(t *testing.T) {
	log := filepath.Join(t.TempDir(), "notifications")
	c := fixture(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	text := fmt.Sprintf("name='notify'\nbackup_dir=%q\n[mysql]\ndump_binary=%q\ndatabases=['demo']\n[dingtalk]\nwebhook='https://dingtalk.example/robot'\nsecret='dummy-secret'\n", c.BackupDir, c.MySQL.DumpBinary)
	for _, scenario := range []string{"success", "dump-failure", "sync-failure"} {
		config := strings.Replace(text, "name='notify'", "name='notify-"+scenario+"'", 1)
		if scenario == "sync-failure" {
			config += "[sync]\ntype='oss'\noss_bucket='example'\noss_prefix='backups'\noss_region='cn-hangzhou'\noss_endpoint='https://oss.example.invalid'\noss_access_key_id='test-id'\noss_access_key_secret='test-secret'\n"
		}
		if e := os.WriteFile(path, []byte(config), 0600); e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelper$")
		cmd.Env = append(os.Environ(), "BACKUP_TOOL_TEST_CLI=1", "BACKUP_TOOL_TEST_CONFIG="+path, "FAKE_NOTIFY_LOG="+log)
		if scenario == "dump-failure" {
			cmd.Env = append(cmd.Env, "FAKE_FAIL=1")
		}
		out, e := cmd.CombinedOutput()
		if (e != nil) != (scenario != "success") {
			t.Fatalf("unexpected exit for %s: %v, %s", scenario, e, out)
		}
		if scenario == "sync-failure" && !strings.Contains(string(out), "同步失败") {
			t.Fatal("sync failure was not logged", string(out))
		}
	}
	b, e := os.ReadFile(log)
	if e != nil || strings.Count(string(b), "sent\n") != 2 {
		t.Fatalf("wanted one notification per failure, got %s, %v", b, e)
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestDingTalkBusinessErrorAndSecretHiding(t *testing.T) {
	before := http.DefaultTransport
	defer func() { http.DefaultTransport = before }()
	http.DefaultTransport = testTransport(func(_ *http.Request) (*http.Response, error) {
		return response(`{"errcode":310000,"errmsg":"bad signature"}`), nil
	})
	c := fixture(t)
	c.DingTalk.Webhook = "https://dingtalk.example/robot"
	c.DingTalk.Secret = "test-secret"
	if e := notifyFailure(context.Background(), c, "error"); e == nil {
		t.Fatal("ignored business error")
	}
	if strings.Contains(redact(c, c.DingTalk.Webhook+" "+c.DingTalk.Secret), "test-secret") {
		t.Fatal("secret not redacted")
	}
}
func TestUnknownTOMLAndConflictingAuth(t *testing.T) {
	for _, text := range []string{"keep_dayz=1", "[mysql]\nsingle_transaction=false", "[mysql]\ndefaults_file='/tmp/client.cnf'\npassword='test'", "[mysql]\npassword='one'\npassword_env='TWO'"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte(text), 0600)
		if _, e := loadConfig(p); e == nil {
			t.Fatal("accepted invalid config", text)
		}
	}
}
