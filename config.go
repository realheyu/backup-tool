package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Name      string   `toml:"name"`
	BackupDir string   `toml:"backup_dir"`
	KeepDays  int      `toml:"keep_days"`
	Timeout   string   `toml:"timeout"`
	MySQL     MySQL    `toml:"mysql"`
	Sync      Sync     `toml:"sync"`
	DingTalk  DingTalk `toml:"dingtalk"`
}
type MySQL struct {
	Host              string   `toml:"host"`
	Port              int      `toml:"port"`
	User              string   `toml:"user"`
	Socket            string   `toml:"socket"`
	DefaultsFile      string   `toml:"defaults_file"`
	Password          *string  `toml:"password"`
	PasswordEnv       string   `toml:"password_env"`
	Databases         []string `toml:"databases"`
	SingleTransaction bool     `toml:"single_transaction"`
	DumpBinary        string   `toml:"dump_binary"`
}
type Sync struct {
	Type               string `toml:"type"`
	OSSBucket          string `toml:"oss_bucket"`
	OSSPrefix          string `toml:"oss_prefix"`
	OSSRegion          string `toml:"oss_region"`
	OSSEndpoint        string `toml:"oss_endpoint"`
	OSSAccessKeyID     string `toml:"oss_access_key_id"`
	OSSAccessKeySecret string `toml:"oss_access_key_secret"`
}
type DingTalk struct {
	Webhook    string `toml:"webhook"`
	Secret     string `toml:"secret"`
	WebhookEnv string `toml:"webhook_env"`
	SecretEnv  string `toml:"secret_env"`
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var safeOSSBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var safeOSSRegion = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func loadConfig(path string) (Config, error) {
	c := Config{Name: "mysql", BackupDir: "./backups", KeepDays: 7, Timeout: "30m", MySQL: MySQL{SingleTransaction: true, DumpBinary: "mysqldump"}, Sync: Sync{Type: "none"}}
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return c, err
	}
	if x := md.Undecoded(); len(x) > 0 {
		return c, fmt.Errorf("未知 TOML 配置项: %v", x)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	if !safeName.MatchString(c.Name) {
		return c, fmt.Errorf("name 只能包含字母、数字、下划线和短横线，最长64字符")
	}
	if c.BackupDir == "" {
		return c, fmt.Errorf("backup_dir 不能为空")
	}
	c.BackupDir = resolve(c.BackupDir)
	if filepath.Clean(c.BackupDir) == "/" {
		return c, fmt.Errorf("backup_dir 不能是根目录")
	}
	if c.KeepDays < 1 || c.KeepDays > 36500 {
		return c, fmt.Errorf("keep_days 应在1到36500之间，不支持永久保留")
	}
	if d, e := time.ParseDuration(c.Timeout); e != nil || d <= 0 {
		return c, fmt.Errorf("timeout 必须是正时长，例如 30m")
	}
	m := &c.MySQL
	if !m.SingleTransaction {
		return c, fmt.Errorf("仅支持事务快照备份，请保持 single_transaction = true；不提供全局锁表模式")
	}
	if m.Port < 0 || m.Port > 65535 {
		return c, fmt.Errorf("mysql.port 无效")
	}
	if m.Socket != "" && (m.Host != "" || m.Port > 0) {
		return c, fmt.Errorf("socket 不能与 host/port 同时设置")
	}
	m.DefaultsFile = resolve(m.DefaultsFile)
	if m.Password != nil && m.PasswordEnv != "" {
		return c, fmt.Errorf("password 和 password_env 只能选一个")
	}
	if m.DefaultsFile != "" && (m.Password != nil || m.PasswordEnv != "") {
		return c, fmt.Errorf("defaults_file 和显式密码只能选一种认证方式")
	}
	if m.DumpBinary == "" {
		return c, fmt.Errorf("dump_binary 不能为空")
	}
	if len(m.Databases) == 0 {
		return c, fmt.Errorf("mysql.databases 必须明确填写要备份的数据库，不能留空")
	}
	seen := map[string]bool{}
	for _, db := range m.Databases {
		if db == "" || strings.HasPrefix(db, "-") || strings.ContainsAny(db, "\x00\r\n") {
			return c, fmt.Errorf("数据库名称无效: %q", db)
		}
		if seen[db] {
			return c, fmt.Errorf("重复数据库: %s", db)
		}
		seen[db] = true
	}
	s := &c.Sync
	switch s.Type {
	case "none":
	case "oss":
		if !safeOSSBucket.MatchString(s.OSSBucket) {
			return c, fmt.Errorf("oss_bucket 需为 Bucket 名称，不含 oss:// 或目录路径")
		}
		s.OSSPrefix = strings.Trim(s.OSSPrefix, "/")
		if strings.ContainsAny(s.OSSPrefix, "\x00\r\n\t ?#\\") || strings.Contains(s.OSSPrefix, "://") {
			return c, fmt.Errorf("oss_prefix 需为目录前缀，不含 URL、空格或控制字符")
		}
		if s.OSSPrefix != "" {
			for _, part := range strings.Split(s.OSSPrefix, "/") {
				if part == "" || part == "." || part == ".." {
					return c, fmt.Errorf("oss_prefix 不能包含空目录、. 或 ..")
				}
			}
		}
		if !safeOSSRegion.MatchString(s.OSSRegion) {
			return c, fmt.Errorf("oss_region 需填写 bucket 所在地域，例如 cn-hangzhou")
		}
		if s.OSSEndpoint != "" {
			if !strings.Contains(s.OSSEndpoint, "://") {
				s.OSSEndpoint = "https://" + s.OSSEndpoint
			}
			u, err := url.Parse(s.OSSEndpoint)
			if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return c, fmt.Errorf("oss_endpoint 需为 HTTP/HTTPS 服务地址，不含路径、凭据或查询参数")
			}
			s.OSSEndpoint = strings.TrimRight(s.OSSEndpoint, "/")
		}
		if (s.OSSAccessKeyID == "") != (s.OSSAccessKeySecret == "") {
			return c, fmt.Errorf("oss_access_key_id 和 oss_access_key_secret 必须同时填写")
		}
	default:
		return c, fmt.Errorf("sync.type 仅支持 none/oss")
	}
	resolveDingTalk(&c)
	return c, nil
}
func resolveDingTalk(c *Config) {
	if c.DingTalk.WebhookEnv != "" {
		c.DingTalk.Webhook = os.Getenv(c.DingTalk.WebhookEnv)
	}
	if c.DingTalk.SecretEnv != "" {
		c.DingTalk.Secret = os.Getenv(c.DingTalk.SecretEnv)
	}
}
func (c Config) timeout() time.Duration { d, _ := time.ParseDuration(c.Timeout); return d }
func (c Config) root() string           { return filepath.Join(c.BackupDir, c.Name) }
