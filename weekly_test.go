package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mockDingTalk(t *testing.T, respond func(string) (*http.Response, error)) {
	t.Helper()
	before := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			MsgType  string `json:"msgtype"`
			Markdown struct {
				Title string `json:"title"`
				Text  string `json:"text"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.MsgType != "markdown" || body.Markdown.Title == "" || body.Markdown.Text == "" {
			t.Fatal("notification must contain a Markdown title and body")
		}
		return respond(body.Markdown.Text)
	})
	t.Cleanup(func() { http.DefaultTransport = before })
}

func notificationFixture(t *testing.T) Config {
	t.Helper()
	c := fixture(t)
	c.DingTalk = DingTalk{Webhook: "https://dingtalk.example/robot", Secret: "test-signing-secret"}
	return c
}

func TestWeeklySummaryCountsAndDeduplicatesAcrossRuns(t *testing.T) {
	c := notificationFixture(t)
	var messages []string
	mockDingTalk(t, func(message string) (*http.Response, error) {
		messages = append(messages, message)
		return response(`{"errcode":0}`), nil
	})
	start := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	for _, run := range []struct {
		after time.Duration
		err   error
	}{
		{0, nil}, {24 * time.Hour, errors.New("dump failed")}, {6 * 24 * time.Hour, nil},
	} {
		if err := recordRunAndNotifyWeekly(c, run.err, start.Add(run.after)); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 0 {
		t.Fatal("ordinary runs triggered a summary early")
	}
	if err := recordRunAndNotifyWeekly(c, nil, start.Add(weeklyInterval)); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "backup-tool 每周汇总") || !strings.Contains(messages[0], "**成功次数**：3\n- **失败次数**：1") || !strings.Contains(messages[0], "**统计开始**：2026-11-05 11:00:00") {
		t.Fatal("incorrect weekly summary", messages)
	}
	for _, at := range []time.Time{start.Add(weeklyInterval + time.Hour), start.Add(2 * weeklyInterval)} {
		if err := recordRunAndNotifyWeekly(c, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 2 || !strings.Contains(messages[1], "**成功次数**：2\n- **失败次数**：0") {
		t.Fatal("summary duplicated or old counters reused", messages)
	}
	path := filepath.Join(c.root(), "notification-state.json")
	state, err := loadNotificationState(path, start.Add(2*weeklyInterval))
	if err != nil || state.Succeeded != 0 || state.Failed != 0 || !state.Since.Equal(start.Add(2*weeklyInterval)) {
		t.Fatal("summary state not reset after delivery", state, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 1024 || info.Mode().Perm() != 0600 {
		t.Fatal("state is not a small private file", info, err)
	}
}

func TestWeeklySummaryFailureRetainsCountersAndRetries(t *testing.T) {
	c := notificationFixture(t)
	attempts := 0
	mockDingTalk(t, func(message string) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return response(`{"errcode":310000,"errmsg":"test delivery failure"}`), nil
		}
		if !strings.Contains(message, "**成功次数**：2\n- **失败次数**：1") {
			t.Fatal("retry lost pending counters", message)
		}
		return response(`{"errcode":0}`), nil
	})
	start := time.Now().Add(-weeklyInterval)
	if err := recordRunAndNotifyWeekly(c, nil, start); err != nil {
		t.Fatal(err)
	}
	if err := recordRunAndNotifyWeekly(c, nil, start.Add(weeklyInterval)); err == nil {
		t.Fatal("weekly delivery failure ignored")
	}
	state, err := loadNotificationState(filepath.Join(c.root(), "notification-state.json"), time.Now())
	if err != nil || state.Succeeded != 2 || !state.Since.Equal(start.UTC()) {
		t.Fatal("failed summary was cleared", state, err)
	}
	if err := recordRunAndNotifyWeekly(c, errors.New("dump failed"), start.Add(weeklyInterval+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := recordRunAndNotifyWeekly(c, nil, start.Add(weeklyInterval+2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("unexpected delivery attempts", attempts)
	}
}

func TestWeeklyStateRecoversStalePartialAndRejectsCorruption(t *testing.T) {
	c := notificationFixture(t)
	now := time.Now()
	if err := recordRunAndNotifyWeekly(c, nil, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.root(), "notification-state.json")
	if err := os.WriteFile(path+".partial", []byte("interrupted write"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recordRunAndNotifyWeekly(c, nil, now.Add(time.Hour)); err != nil {
		t.Fatal("stale temporary state blocked future summaries", err)
	}
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Fatal("temporary state retained")
	}
	for _, data := range []string{"broken JSON", "{}", `{"format":2}`, strings.Repeat("x", 8193)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := recordRunAndNotifyWeekly(c, nil, now); err == nil {
			t.Fatal("corrupt notification state silently accepted")
		}
	}
}

func TestMissingDingTalkConfigurationIsAnError(t *testing.T) {
	c := fixture(t)
	if err := notifyMessage(t.Context(), c, "失败", "error"); err == nil {
		t.Fatal("missing webhook reported as delivered")
	}
	// Verify transport failures cannot accidentally count as successful sends.
	c = notificationFixture(t)
	mockDingTalk(t, func(_ string) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
	if err := notifyMessage(t.Context(), c, "每周汇总", "counts"); err == nil {
		t.Fatal("transport failure ignored")
	}
}
