package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
)

func ossFixture() Sync {
	return Sync{Type: "oss", OSSBucket: "example", OSSPrefix: "backups", OSSRegion: "cn-hangzhou",
		OSSAccessKeyID: "test-access-key", OSSAccessKeySecret: "test-access-secret"}
}

func mockOSS(t *testing.T, transport testTransport) {
	t.Helper()
	before := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := transport(r)
		if resp != nil {
			resp.Request = r
			resp.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return resp, err
	})
	t.Cleanup(func() { http.DefaultTransport = before })
}

func ossUploadResponse(body []byte) *http.Response {
	h := oss.NewCRC64(0)
	h.Write(body)
	resp := response("")
	resp.Header.Set("X-Oss-Hash-Crc64ecma", strconv.FormatUint(h.Sum64(), 10))
	resp.Header.Set("ETag", `"test-etag"`)
	return resp
}

func TestOSSUploadsPayloadsAndManifestLast(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, wantPrefix string
		env                      bool
	}{
		{"nested prefix", "/backups/mysql/", "backups/mysql/", false},
		{"bucket root", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Sync = ossFixture()
			c.Sync.OSSPrefix = tc.prefix
			// Leftover STS environment variables must never enter requests.
			t.Setenv("OSS_SESSION_TOKEN", "ignored-session-token")
			host := "example.oss-cn-hangzhou.aliyuncs.com"
			if tc.env {
				t.Setenv("OSS_ACCESS_KEY_ID", c.Sync.OSSAccessKeyID)
				t.Setenv("OSS_ACCESS_KEY_SECRET", c.Sync.OSSAccessKeySecret)
				c.Sync.OSSAccessKeyID, c.Sync.OSSAccessKeySecret = "", ""
				c.Sync.OSSEndpoint = "https://oss-cn-hangzhou-internal.aliyuncs.com"
				host = "example.oss-cn-hangzhou-internal.aliyuncs.com"
			}
			var keys []string
			payloads := map[string][]byte{}
			mockOSS(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPut || r.URL.Host != host || r.URL.Scheme != "https" {
					t.Fatalf("unexpected upload: %s %s", r.Method, r.URL)
				}
				auth := r.Header.Get("Authorization")
				if !strings.HasPrefix(auth, "OSS4-HMAC-SHA256 Credential=test-access-key/") || !strings.Contains(auth, "/cn-hangzhou/oss/aliyun_v4_request") {
					t.Fatal("missing signed OSS credentials")
				}
				if r.Header.Get("X-Oss-Security-Token") != "" {
					t.Fatal("temporary credentials unexpectedly included in upload")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				keys = append(keys, r.URL.Path)
				payloads[r.URL.Path] = body
				return ossUploadResponse(body), nil
			})
			if err := runBackup(context.Background(), c, nil); err != nil {
				t.Fatal(err)
			}
			dir := runs(t, c)[0]
			prefix := "/" + tc.wantPrefix + c.Name + "/" + filepath.Base(dir) + "/"
			want := []string{prefix + "001-demo.sql.gz", prefix + "002-payments.sql.gz", prefix + "backup.json"}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("upload order: got %v, want %v", keys, want)
			}
			for _, key := range want {
				local, err := os.ReadFile(filepath.Join(dir, filepath.Base(key)))
				if err != nil || !bytes.Equal(local, payloads[key]) {
					t.Fatalf("uploaded payload differs from local backup: %s (%v)", key, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "synced.json")); err != nil {
				t.Fatal("successful upload not marked as synchronized", err)
			}
		})
	}
}

func TestOSSUploadDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			c := fixture(t)
			c.Sync = ossFixture()
			c.Timeout = "50ms"
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "backup.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			mockOSS(t, func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.DeadlineExceeded
			if canceled {
				cancel()
				want = context.Canceled
			}
			start := time.Now()
			err := syncBackup(ctx, c, dir, BackupInfo{ID: "test-run"})
			if !errors.Is(err, want) || time.Since(start) > 2*time.Second {
				t.Fatalf("upload did not honor context: %v", err)
			}
		})
	}
}

func TestOSSRejectsCRCMismatch(t *testing.T) {
	c := fixture(t)
	c.Sync = ossFixture()
	mockOSS(t, func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		resp := ossUploadResponse(body)
		resp.Header.Set("X-Oss-Hash-Crc64ecma", "1")
		return resp, nil
	})
	if err := runBackup(context.Background(), c, nil); err == nil {
		t.Fatal("upload accepted a CRC mismatch")
	}
	dir := runs(t, c)[0]
	if _, err := os.Stat(filepath.Join(dir, "synced.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt upload marked as synchronized")
	}
}

func TestOSSLargeFileMultipartCompletesBeforeManifest(t *testing.T) {
	c := fixture(t)
	c.Sync = ossFixture()
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), 7*1024*1024)
	for name, body := range map[string][]byte{"large.sql.gz": data, "backup.json": []byte("{}\n")} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	parts := map[int][]byte{}
	completed, manifest := false, false
	mockOSS(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			return response(`<InitiateMultipartUploadResult><Bucket>example</Bucket><Key>backups/test/test-run/large.sql.gz</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`), nil
		case r.Method == http.MethodPut && q.Get("uploadId") == "test-upload":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(q.Get("partNumber"))
			if err != nil {
				return nil, err
			}
			parts[n] = body
			return ossUploadResponse(body), nil
		case r.Method == http.MethodPost && q.Get("uploadId") == "test-upload":
			if len(parts) != 2 || !bytes.Equal(append(append([]byte{}, parts[1]...), parts[2]...), data) {
				return nil, fmt.Errorf("multipart payload mismatch")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || !strings.Contains(string(body), "<PartNumber>1</PartNumber>") || !strings.Contains(string(body), "<PartNumber>2</PartNumber>") {
				return nil, fmt.Errorf("invalid multipart completion request: %v", err)
			}
			completed = true
			resp := ossUploadResponse(data)
			resp.Body = io.NopCloser(strings.NewReader(`<CompleteMultipartUploadResult><ETag>test-etag</ETag></CompleteMultipartUploadResult>`))
			return resp, nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/backup.json") && completed:
			manifest = true
			body, err := io.ReadAll(r.Body)
			return ossUploadResponse(body), err
		default:
			return nil, fmt.Errorf("unexpected multipart request: %s %s", r.Method, r.URL)
		}
	})
	if err := syncBackup(context.Background(), c, dir, BackupInfo{ID: "test-run", Files: []FileInfo{{Name: "large.sql.gz"}}}); err != nil {
		t.Fatal(err)
	}
	if !completed || !manifest {
		t.Fatal("multipart upload or manifest missing")
	}
}

func TestOSSMultipartFailureDoesNotReadListOrAbort(t *testing.T) {
	c := fixture(t)
	c.Sync = ossFixture()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "large.sql.gz"), bytes.Repeat([]byte("x"), 7*1024*1024), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	partAttempts := 0
	mockOSS(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			return response(`<InitiateMultipartUploadResult><Bucket>example</Bucket><Key>backups/test/test-run/large.sql.gz</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`), nil
		}
		if r.Method == http.MethodPut && r.URL.Query().Get("uploadId") == "test-upload" {
			partAttempts++
		} else {
			t.Errorf("unexpected operation after multipart failure: %s", r.Method)
		}
		resp := response(`<Error><Code>AccessDenied</Code><Message>expected test failure</Message><RequestId>test</RequestId></Error>`)
		resp.StatusCode = http.StatusForbidden
		return resp, nil
	})
	if err := syncBackup(context.Background(), c, dir, BackupInfo{ID: "test-run", Files: []FileInfo{{Name: "large.sql.gz"}}}); err == nil {
		t.Fatal("multipart failure ignored")
	}
	if partAttempts == 0 {
		t.Fatal("multipart upload was not exercised")
	}
}

func TestOSSConfigValidation(t *testing.T) {
	mysql := "[mysql]\ndatabases=['demo']\n"
	base := "[sync]\ntype='oss'\noss_bucket='example'\noss_prefix='/backups/'\noss_region='cn-hangzhou'\n"
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"environment", base, true},
		{"static", base + "oss_access_key_id='test-id'\noss_access_key_secret='test-secret'", true},
		{"endpoint", base + "oss_endpoint='oss-cn-hangzhou-internal.aliyuncs.com/'", true},
		{"missing region", strings.ReplaceAll(base, "oss_region='cn-hangzhou'\n", ""), false},
		{"missing bucket", strings.ReplaceAll(base, "oss_bucket='example'\n", ""), false},
		{"empty bucket", strings.ReplaceAll(base, "oss_bucket='example'", "oss_bucket=''"), false},
		{"bad bucket", strings.ReplaceAll(base, "oss_bucket='example'", "oss_bucket='Bad_Bucket'"), false},
		{"bucket URI", strings.ReplaceAll(base, "oss_bucket='example'", "oss_bucket='oss://example'"), false},
		{"bucket path", strings.ReplaceAll(base, "oss_bucket='example'", "oss_bucket='example/mysql'"), false},
		{"missing prefix", strings.ReplaceAll(base, "oss_prefix='/backups/'\n", ""), true},
		{"empty prefix", strings.ReplaceAll(base, "oss_prefix='/backups/'", "oss_prefix=''"), true},
		{"root prefix", strings.ReplaceAll(base, "oss_prefix='/backups/'", "oss_prefix='/'"), true},
		{"prefix space", strings.ReplaceAll(base, "/backups/", "my backups"), false},
		{"prefix parent", strings.ReplaceAll(base, "/backups/", "backups/../mysql"), false},
		{"prefix current", strings.ReplaceAll(base, "/backups/", "backups/./mysql"), false},
		{"prefix empty segment", strings.ReplaceAll(base, "/backups/", "backups//mysql"), false},
		{"prefix URL", strings.ReplaceAll(base, "/backups/", "oss://example/mysql"), false},
		{"only id", base + "oss_access_key_id='test-id'", false},
		{"only secret", base + "oss_access_key_secret='test-secret'", false},
		{"removed token", base + "oss_access_key_id='test-id'\noss_access_key_secret='test-secret'\noss_session_token='test-token'", false},
		{"removed empty token", base + "oss_session_token=''", false},
		{"endpoint path", base + "oss_endpoint='https://oss.example/path'", false},
		{"endpoint credentials", base + "oss_endpoint='https://user:password@oss.example'", false},
		{"endpoint scheme", base + "oss_endpoint='ftp://oss.example'", false},
		{"old binary", base + "oss_binary='ossutil'", false},
		{"old config", base + "oss_config='/tmp/ossconfig'", false},
		{"old URI", base + "oss_uri='oss://example/backups'", false},
		{"removed ssh type", "[sync]\ntype='ssh'", false},
		{"removed ssh host", "[sync]\ntype='none'\nssh_host=''", false},
		{"removed ssh dir", "[sync]\ntype='none'\nssh_dir=''", false},
		{"removed ssh port", "[sync]\ntype='none'\nssh_port=22", false},
		{"removed ssh key", "[sync]\ntype='none'\nssh_key=''", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(mysql+tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.valid {
				prefix := "backups"
				if tc.name == "missing prefix" || tc.name == "empty prefix" || tc.name == "root prefix" {
					prefix = ""
				}
				if c.Sync.OSSBucket != "example" || c.Sync.OSSPrefix != prefix || (tc.name == "endpoint" && c.Sync.OSSEndpoint != "https://oss-cn-hangzhou-internal.aliyuncs.com") {
					t.Fatal("OSS target normalization failed")
				}
			}
		})
	}
}

func TestOSSCredentialsRedacted(t *testing.T) {
	c := fixture(t)
	c.Sync = ossFixture()
	t.Setenv("OSS_ACCESS_KEY_ID", "env-access-id")
	t.Setenv("OSS_ACCESS_KEY_SECRET", "env-access-secret")
	for _, secret := range []string{c.Sync.OSSAccessKeyID, c.Sync.OSSAccessKeySecret,
		os.Getenv("OSS_ACCESS_KEY_ID"), os.Getenv("OSS_ACCESS_KEY_SECRET")} {
		if strings.Contains(redact(c, "OSS error: "+secret), secret) {
			t.Fatal("OSS credential leaked")
		}
	}
}

func TestOSSMissingAccessKeyFailsBeforeUpload(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"test-id", ""}, {"", "test-secret"}} {
		t.Run(fmt.Sprint(pair[0] != "", pair[1] != ""), func(t *testing.T) {
			c := fixture(t)
			c.Sync = ossFixture()
			c.Sync.OSSAccessKeyID, c.Sync.OSSAccessKeySecret = "", ""
			t.Setenv("OSS_ACCESS_KEY_ID", pair[0])
			t.Setenv("OSS_ACCESS_KEY_SECRET", pair[1])
			mockOSS(t, func(_ *http.Request) (*http.Response, error) {
				t.Fatal("incomplete credentials reached upload transport")
				return nil, nil
			})
			if err := syncBackup(context.Background(), c, t.TempDir(), BackupInfo{ID: "test-run"}); err == nil {
				t.Fatal("missing AccessKey credentials accepted")
			}
		})
	}
}
