//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func passwordFor(ctx context.Context, c Config, ask bool) (*string, error) {
	if ask {
		if c.MySQL.DefaultsFile != "" {
			return nil, fmt.Errorf("--ask-password 不与 defaults_file 同时使用")
		}
		p, e := readPassword(ctx)
		return &p, e
	}
	if c.MySQL.PasswordEnv != "" {
		p, ok := os.LookupEnv(c.MySQL.PasswordEnv)
		if !ok {
			return nil, fmt.Errorf("密码环境变量 %s 未设置", c.MySQL.PasswordEnv)
		}
		return &p, nil
	}
	return c.MySQL.Password, nil
}
func readPassword(ctx context.Context) (string, error) {
	f, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if e != nil {
		// An inherited terminal can still be usable in containers/sandboxes
		// where opening /dev/tty is restricted. stty rejects non-terminal input.
		f = os.Stdin
	} else {
		defer f.Close()
	}
	setEcho := func(arg string) error { cmd := exec.Command("stty", arg); cmd.Stdin = f; return cmd.Run() }
	stateCmd := exec.Command("stty", "-g")
	stateCmd.Stdin = f
	state, e := stateCmd.Output()
	if e != nil {
		return "", fmt.Errorf("无法读取终端状态，请改用 password_env 或客户端配置: %w", e)
	}
	if e = setEcho("-echo"); e != nil {
		return "", e
	}
	defer func() { _ = setEcho(strings.TrimSpace(string(state))); fmt.Fprintln(os.Stderr) }()
	fmt.Fprint(os.Stderr, "MySQL 密码: ")
	type result struct {
		s string
		e error
	}
	ch := make(chan result, 1)
	go func() { s, e := bufio.NewReader(f).ReadString('\n'); ch <- result{s, e} }()
	select {
	case r := <-ch:
		if r.e != nil {
			return "", r.e
		}
		return strings.TrimSuffix(strings.TrimSuffix(r.s, "\n"), "\r"), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func passwordConfig(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("密码不能包含 NUL")
	}
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\b", "\\b")
	return "[client]\npassword=\"" + r.Replace(p) + "\"\n", nil
}

// Explicit passwords are supplied in a 0600 temporary option file, never as
// command-line passwords or MYSQL_PWD. mysqldump always runs locally.
func dumpCommand(c Config, password *string, db string) (string, []string, func(), error) {
	m := c.MySQL
	binary := m.DumpBinary
	var first []string
	cleanup := func() {}
	if password != nil {
		text, e := passwordConfig(*password)
		if e != nil {
			return "", nil, cleanup, e
		}
		f, e := os.CreateTemp("", "backup-tool-*.cnf")
		if e != nil {
			return "", nil, cleanup, e
		}
		cleanup = func() { _ = os.Remove(f.Name()) }
		if _, e = f.WriteString(text); e != nil {
			f.Close()
			cleanup()
			return "", nil, func() {}, e
		}
		if e = f.Close(); e != nil {
			cleanup()
			return "", nil, func() {}, e
		}
		first = []string{"--defaults-file=" + f.Name()}
	} else if m.DefaultsFile != "" {
		first = []string{"--defaults-file=" + m.DefaultsFile}
	}
	args := append(first, "--quick", "--hex-blob", "--routines", "--events", "--triggers", "--no-tablespaces", "--set-gtid-purged=OFF", "--single-transaction", "--skip-lock-tables")
	if m.Host != "" {
		args = append(args, "--host="+m.Host, "--protocol=TCP")
	}
	if m.Port > 0 {
		args = append(args, fmt.Sprintf("--port=%d", m.Port), "--protocol=TCP")
	}
	if m.User != "" {
		args = append(args, "--user="+m.User)
	}
	if m.Socket != "" {
		args = append(args, "--socket="+m.Socket)
	}
	args = append(args, "--databases", db)
	return binary, args, cleanup, nil
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > 8192 {
		p = p[len(p)-8192:]
	}
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = b.data[len(b.data)-8192:]
	}
	return n, nil
}
func execute(ctx context.Context, c Config, binary string, args []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = out
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", binary, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%s: %w; %s", binary, err, strings.TrimSpace(string(stderr.data)))
	}
	return nil
}
