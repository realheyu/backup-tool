package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectDatabaseConnections(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         []string
	}{
		{"local", "host='127.0.0.1'\nport=3306", []string{"--host=127.0.0.1", "--protocol=TCP", "--port=3306", "--protocol=TCP", "--user=backup"}},
		{"remote", "host='10.0.0.10'\nport=3306", []string{"--host=10.0.0.10", "--protocol=TCP", "--port=3306", "--protocol=TCP", "--user=backup"}},
		{"docker mapped port", "host='127.0.0.1'\nport=13306", []string{"--host=127.0.0.1", "--protocol=TCP", "--port=13306", "--protocol=TCP", "--user=backup"}},
		{"localhost TCP", "host='localhost'\nport=3307", []string{"--host=localhost", "--protocol=TCP", "--port=3307", "--protocol=TCP", "--user=backup"}},
		{"local socket", "socket='/tmp/mysql.sock'", []string{"--user=backup", "--socket=/tmp/mysql.sock"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			p := filepath.Join(t.TempDir(), "config.toml")
			config := fmt.Sprintf("name='direct'\nbackup_dir=%q\n[mysql]\ndump_binary=%q\nuser='backup'\npassword='test-password'\ndatabases=['app']\n%s\n", c.BackupDir, c.MySQL.DumpBinary, tc.fields)
			if err := os.WriteFile(p, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(t.TempDir(), "args")
			t.Setenv("FAKE_ARGS", log)
			if err := runBackup(context.Background(), c, c.MySQL.Password); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			want := append(append([]string{}, tc.want...), "--databases", "app")
			if len(args) < len(want) || !reflect.DeepEqual(args[len(args)-len(want):], want) {
				t.Fatalf("connection arguments changed: got %v, want suffix %v", args, want)
			}
			if !strings.HasPrefix(args[0], "--defaults-file=") || strings.Contains(string(data), *c.MySQL.Password) {
				t.Fatal("explicit password not supplied through an option file")
			}
			if got := runs(t, c); len(got) != 1 {
				t.Fatal("missing local backup", got)
			}
		})
	}
}

func TestRemovedSourceSettingsRejected(t *testing.T) {
	for _, fields := range []string{
		"container=''", "container='mysql1'", "ssh_host=''", "ssh_host='backup@source'", "ssh_port=22", "ssh_key='keys/source'",
		"separate=true", "separate=false",
	} {
		t.Run(fields, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(p, []byte("[mysql]\n"+fields), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "未知 TOML 配置项") {
				t.Fatal("obsolete backup source setting accepted or silently ignored", err)
			}
		})
	}
}

func TestClientConfigPathsAreLocal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[mysql]\nhost='10.0.0.10'\ndatabases=['demo']\ndefaults_file='credentials/client.cnf'"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(p), "credentials/client.cnf")
	if c.MySQL.DefaultsFile != want {
		t.Fatal("client config not resolved against local TOML directory", c.MySQL.DefaultsFile)
	}
}

func TestDatabaseListCannotBeOmittedOrEmpty(t *testing.T) {
	for _, fields := range []string{"", "databases=[]"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte("[mysql]\n"+fields), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "mysql.databases") {
			t.Fatal("missing database list did not report a configuration error", err)
		}
	}
	c := fixture(t)
	c.MySQL.Databases = nil
	if err := runBackup(context.Background(), c, nil); err == nil {
		t.Fatal("empty database list accepted by backup runner")
	}
}
