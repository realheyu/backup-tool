package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const weeklyInterval = 7 * 24 * time.Hour

// One bounded, overwritten file survives cron invocations without a run log.
type notificationState struct {
	Format        int       `json:"format"`
	Since         time.Time `json:"since"`
	Succeeded     int64     `json:"succeeded"`
	Failed        int64     `json:"failed"`
	LastRunAt     time.Time `json:"last_run_at"`
	LastSuccessAt time.Time `json:"last_success_at"`
}

func recordRunAndNotifyWeekly(c Config, runErr error, now time.Time) error {
	if err := ensureDir(c.root()); err != nil {
		return fmt.Errorf("准备周汇总目录: %w", err)
	}
	// A separate lock also allows a rejected concurrent backup to be counted.
	lock, err := os.OpenFile(filepath.Join(c.root(), ".notify.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("打开周汇总锁: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("周汇总已有任务正在更新: %w", err)
	}
	path := filepath.Join(c.root(), "notification-state.json")
	state, err := loadNotificationState(path, now)
	if err != nil {
		return err
	}
	state.LastRunAt = now.UTC()
	if runErr == nil {
		state.Succeeded++
		state.LastSuccessAt = now.UTC()
	} else {
		state.Failed++
	}
	// Save counters before sending; failed requests leave the entire window
	// available for the next invocation to retry.
	if err := saveNotificationState(path, state); err != nil {
		return err
	}
	if now.Sub(state.Since) < weeklyInterval {
		return nil
	}
	lastSuccess := "尚无成功记录"
	if !state.LastSuccessAt.IsZero() {
		lastSuccess = notificationTime(state.LastSuccessAt)
	}
	message := fmt.Sprintf("#### 本周统计\n\n- **执行次数**：%d\n- **成功次数**：%d\n- **失败次数**：%d\n\n#### 时间记录（UTC+8）\n\n- **统计开始**：%s\n- **统计截止**：%s\n- **最近成功**：%s\n- **最近执行**：%s",
		state.Succeeded+state.Failed, state.Succeeded, state.Failed,
		notificationTime(state.Since), notificationTime(now), lastSuccess, notificationTime(now))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := notifyMessage(ctx, c, "每周汇总", message); err != nil {
		return fmt.Errorf("发送每周汇总: %w", err)
	}
	state.Since = now.UTC()
	state.Succeeded, state.Failed = 0, 0
	if err := saveNotificationState(path, state); err != nil {
		return err
	}
	fmt.Println("已发送每周汇总")
	return nil
}

func loadNotificationState(path string, now time.Time) (notificationState, error) {
	state := notificationState{Format: 1, Since: now.UTC()}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("读取周汇总状态: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 8192 {
		return state, fmt.Errorf("周汇总状态文件类型或大小无效: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, fmt.Errorf("读取周汇总状态: %w", err)
	}
	state = notificationState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析周汇总状态: %w", err)
	}
	if state.Format != 1 || state.Since.IsZero() || state.Succeeded < 0 || state.Failed < 0 {
		return state, fmt.Errorf("周汇总状态无效: %s", path)
	}
	return state, nil
}

func saveNotificationState(path string, state notificationState) error {
	// The lock makes it safe to remove a temporary state file left by a crash.
	if err := os.Remove(path + ".partial"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理周汇总临时状态: %w", err)
	}
	if err := writeJSON(path, state); err != nil {
		return fmt.Errorf("保存周汇总状态: %w", err)
	}
	return nil
}
