package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarkdownFailureKeepsErrorsLiteralAndPrivate(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiline", true: "long-unicode"}[long], func(t *testing.T) {
			c := notificationFixture(t)
			password := "private_*_password"
			c.MySQL.Password = &password
			c.Sync.OSSAccessKeyID = "private-access-id"
			c.Sync.OSSAccessKeySecret = "private_*_access-secret"
			secrets := []string{password, c.DingTalk.Webhook, c.DingTalk.Secret, c.Sync.OSSAccessKeyID, c.Sync.OSSAccessKeySecret}
			message := strings.Join(secrets, " ") + "\r\n# error *details* [link](unsafe)\r\nnext line"
			if long {
				message += "\n" + strings.Repeat("数据库导出失败🌏", 1000)
			}
			called := false
			before := http.DefaultTransport
			http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
				called = true
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					MsgType  string          `json:"msgtype"`
					Text     json.RawMessage `json:"text"`
					Markdown struct {
						Title string `json:"title"`
						Text  string `json:"text"`
					} `json:"markdown"`
				}
				if json.Unmarshal(data, &body) != nil || body.MsgType != "markdown" || body.Markdown.Title == "" || len(body.Text) != 0 {
					t.Fatal("invalid Markdown wire payload")
				}
				if !strings.Contains(body.Markdown.Title, c.Name) || !strings.Contains(body.Markdown.Text, "**任务**："+markdownEscape(c.Name)) || !strings.Contains(body.Markdown.Text, "UTC+8") {
					t.Fatal("notification lost task identity or explicit timezone")
				}
				for _, secret := range secrets {
					if strings.Contains(body.Markdown.Text, secret) || strings.Contains(body.Markdown.Text, markdownEscape(secret)) || strings.Contains(body.Markdown.Title, secret) {
						t.Fatal("notification contains credentials")
					}
				}
				if !strings.Contains(body.Markdown.Text, "\n> \\# error \\*details\\* \\[link\\]\\(unsafe\\)\n> next line") {
					t.Fatal("multiline error was lost or interpreted as Markdown")
				}
				if len(body.Markdown.Text) > 3500 || !utf8.ValidString(body.Markdown.Text) {
					t.Fatal("message exceeds byte bound or cuts a UTF-8 character")
				}
				if long && !strings.Contains(body.Markdown.Text, "内容过长，已截断") {
					t.Fatal("truncation was not visible")
				}
				if r.Header.Get("Content-Type") != "application/json" || r.URL.Query().Get("sign") == "" {
					t.Fatal("notification lost JSON content type or signature")
				}
				return response(`{"errcode":0}`), nil
			})
			t.Cleanup(func() { http.DefaultTransport = before })
			if err := notifyFailure(context.Background(), c, message); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("notification not sent")
			}
		})
	}
}
