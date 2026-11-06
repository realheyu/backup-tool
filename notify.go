package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func notifyFailure(ctx context.Context, c Config, message string) error {
	message = strings.ReplaceAll(strings.ReplaceAll(redact(c, message), "\r\n", "\n"), "\r", "\n")
	details := "> " + strings.ReplaceAll(markdownEscape(message), "\n", "\n> ")
	return notifyMessage(ctx, c, "失败", "#### 错误详情\n\n"+details)
}

func markdownEscape(text string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
		"[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", ">", "\\>",
		"<", "\\<", "!", "\\!", "-", "\\-", "+", "\\+", "|", "\\|").Replace(text)
}

func notificationTime(at time.Time) string {
	return at.In(backupZone).Format("2006-01-02 15:04:05")
}

func limitNotification(text string) string {
	// Bound UTF-8 bytes while leaving room for a visible truncation marker.
	const maxBytes = 3500
	const suffix = "\n\n…（内容过长，已截断）"
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes - len(suffix)
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}

func notifyMessage(ctx context.Context, c Config, kind, message string) error {
	d := c.DingTalk
	if d.Webhook == "" {
		if d.WebhookEnv != "" {
			return fmt.Errorf("钉钉环境变量 %s 未设置", d.WebhookEnv)
		}
		return fmt.Errorf("未配置钉钉 webhook，无法发送通知")
	}
	if d.SecretEnv != "" && d.Secret == "" {
		return fmt.Errorf("钉钉签名环境变量 %s 未设置", d.SecretEnv)
	}
	u, e := url.Parse(d.Webhook)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("钉钉 webhook URL 无效")
	}
	if d.Secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		h := hmac.New(sha256.New, []byte(d.Secret))
		h.Write([]byte(ts + "\n" + d.Secret))
		q := u.Query()
		q.Set("timestamp", ts)
		q.Set("sign", base64.StdEncoding.EncodeToString(h.Sum(nil)))
		u.RawQuery = q.Encode()
	}
	host, _ := os.Hostname()
	icon := "❌"
	if kind == "每周汇总" {
		icon = "📊"
	}
	title := redact(c, fmt.Sprintf("backup-tool %s · %s", kind, c.Name))
	text := fmt.Sprintf("### %s backup-tool %s\n\n- **任务**：%s\n- **服务器**：%s\n- **时间（UTC+8）**：%s\n\n%s",
		icon, markdownEscape(kind), markdownEscape(redact(c, c.Name)), markdownEscape(redact(c, host)), notificationTime(time.Now()), message)
	text = limitNotification(redact(c, text))
	body, _ := json.Marshal(map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": title, "text": text}})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if e != nil {
		return fmt.Errorf("构造钉钉请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return fmt.Errorf("拒绝重定向") }}
	resp, e := client.Do(req)
	if e != nil {
		return fmt.Errorf("钉钉请求失败或超时（URL 已隐藏）")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("钉钉 HTTP 状态 %d", resp.StatusCode)
	}
	var result struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result); e != nil {
		return fmt.Errorf("钉钉返回无效 JSON")
	}
	if result.ErrCode == nil {
		return fmt.Errorf("钉钉返回缺少 errcode")
	}
	if *result.ErrCode != 0 {
		return fmt.Errorf("钉钉业务错误 %d: %s", *result.ErrCode, result.ErrMsg)
	}
	return nil
}
