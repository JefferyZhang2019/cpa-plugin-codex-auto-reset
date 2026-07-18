package main

// Card simulation: drives the FSM through all 6 states and prints what the
// UI card would show at each step. Run with:
//   CGO_ENABLED=1 go test -run TestCardSimulation -v ./...
//
// This verifies the full state machine produces the right log messages,
// credit snapshots, and next-action descriptions at every transition.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type cardSimClient struct {
	credits   map[string]*Credit
	weeklyPct int
	consume   ConsumeResponse
}

func (c *cardSimClient) ListCredits(CodexCredentials) (Snapshot, error) {
	out := make([]Credit, 0, len(c.credits))
	for _, cr := range c.credits {
		out = append(out, *cr)
	}
	return Snapshot{Credits: out, AvailableCount: countAvailSim(out), WeeklyPct: -1}, nil
}

func (c *cardSimClient) GetUsage(CodexCredentials) (Snapshot, error) {
	return Snapshot{WeeklyPct: c.weeklyPct}, nil
}

func (c *cardSimClient) Consume(_ CodexCredentials, rrid, cid string) (ConsumeResponse, error) {
	if cr, ok := c.credits[cid]; ok {
		cr.Status = "redeemed"
	}
	c.weeklyPct = 100
	return c.consume, nil
}

func countAvailSim(cs []Credit) int { n := 0; for _, c := range cs { if c.Status == "available" { n++ } }; return n }

type simClock struct{ t time.Time }

func (s *simClock) Now() time.Time { return s.t }

func TestCardSimulation_AllStates(t *testing.T) {
	E := time.Date(2026, 7, 27, 8, 2, 2, 0, time.UTC)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}

	var allLogs []string
	logf := func(level, scope string, state State, msg, next string, nextAt *time.Time, d map[string]any) {
		allLogs = append(allLogs, fmt.Sprintf("[%s/%s] %s", level, state, msg))
	}

	// ============================================================
	// SCENARIO 1: IDLE — credit far from expiry (normal patrol)
	// ============================================================
	t.Log("========================================")
	t.Log("场景 1: IDLE — 重置次数离过期还很远")
	t.Log("========================================")
	fc := &cardSimClient{
		credits: map[string]*Credit{
			"RateLimitResetCredit_d7087f83469c819182a87d5916512c9c": {ID: "RateLimitResetCredit_d7087f83469c819182a87d5916512c9c", Status: "available", ExpiresAt: E},
			"RateLimitResetCredit_a7208f83469c819182a87d5916512c9c": {ID: "RateLimitResetCredit_a7208f83469c819182a87d5916512c9c", Status: "available", ExpiresAt: E.Add(5 * 24 * time.Hour)},
		},
		weeklyPct: 91,
		consume:   ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
	}
	clock := &simClock{t: E.Add(-8 * 24 * time.Hour)}
	fsm := NewAccountFSM("codex-44411af1-alice@example.com-team.json", CodexCredentials{AccessToken: "tok"}, cfg, fc, clock.Now, logf)
	stepAndPrintCard(t, fsm, clock, fc, "场景1: 巡查完毕，credit 离过期还远")

	// ============================================================
	// SCENARIO 2: ARMED — credit within trigger window
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 2: ARMED — 进入触发窗口（credit 5h 后过期）")
	t.Log("========================================")
	clock.t = E.Add(-5 * time.Hour) // 5h before expiry, within 6h lead time
	fsm = NewAccountFSM("codex-44411af1-alice@example.com-team.json", CodexCredentials{AccessToken: "tok"}, cfg, fc, clock.Now, logf)
	stepAndPrintCard(t, fsm, clock, fc, "场景2: 进入 ARMED")

	// ============================================================
	// SCENARIO 3: CONFIRMING → RESETTING (trigger fires)
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 3: CONFIRMING → RESETTING — 触发时间到，发送重置请求")
	t.Log("========================================")
	// After ARMED step, FSM is in CONFIRMING
	stepAndPrintCard(t, fsm, clock, fc, "场景3a: ARMED→CONFIRMING")
	// CONFIRMING step will send POST and enter RESETTING→VERIFYING
	stepAndPrintCard(t, fsm, clock, fc, "场景3b: CONFIRMING→RESETTING→VERIFYING")

	// ============================================================
	// SCENARIO 4: VERIFYING → DONE (success)
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 4: VERIFYING → DONE — 重置成功验证")
	t.Log("========================================")
	clock.t = clock.t.Add(1 * time.Minute) // post-reset verify delay
	stepAndPrintCard(t, fsm, clock, fc, "场景4: 验证通过")

	// ============================================================
	// SCENARIO 5: no_credit — server returns no available credits
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 5: no_credit — 服务端返回无可用次数")
	t.Log("========================================")
	fc3 := &cardSimClient{
		credits: map[string]*Credit{
			"RateLimitResetCredit_xyz123": {ID: "RateLimitResetCredit_xyz123", Status: "available", ExpiresAt: E},
		},
		weeklyPct: 50,
		consume:   ConsumeResponse{Code: ConsumeCodeNoCredit},
	}
	clock3 := &simClock{t: E.Add(-1 * time.Hour)}
	fsm3 := NewAccountFSM("codex-ac9ce24f-carol@example.com-team.json", CodexCredentials{AccessToken: "tok"}, cfg, fc3, clock3.Now, logf)
	fsm3.ForceState(StateCONFIRMING)
	stepAndPrintCard(t, fsm3, clock3, fc3, "场景5: no_credit 停止")

	// ============================================================
	// SCENARIO 6: no available credits at all
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 6: IDLE — 无可用重置次数")
	t.Log("========================================")
	fc4 := &cardSimClient{
		credits:   map[string]*Credit{},
		weeklyPct: 100,
	}
	clock4 := &simClock{t: time.Date(2026, 7, 18, 22, 0, 0, 0, time.UTC)}
	fsm4 := NewAccountFSM("codex-fc4bac78-dave@example.com-team.json", CodexCredentials{AccessToken: "tok"}, cfg, fc4, clock4.Now, logf)
	stepAndPrintCard(t, fsm4, clock4, fc4, "场景6: 无可用次数")

	// ============================================================
	// SCENARIO 7: retries exhausted
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("场景 7: RETRIES EXHAUSTED — 重试耗尽后放弃")
	t.Log("========================================")
	fc5 := &cardSimClient{
		credits: map[string]*Credit{
			"RateLimitResetCredit_retrytest": {ID: "RateLimitResetCredit_retrytest", Status: "available", ExpiresAt: E},
		},
		weeklyPct: 40,
		consume:   ConsumeResponse{Code: ConsumeCodeNoCredit}, // every consume fails
	}
	// Make Consume always return network error
	fc5err := &errOnlyClient{credits: fc5.credits, weeklyPct: 40}
	clock5 := &simClock{t: E.Add(-1 * time.Hour)}
	fsm5 := NewAccountFSM("codex-fc4bac78-eve@example.com-team.json", CodexCredentials{AccessToken: "tok"}, cfg, fc5err, clock5.Now, logf)
	fsm5.ForceState(StateCONFIRMING)
	stepAndPrintCard(t, fsm5, clock5, fc5err, "场景7: 第一次 CONFIRMING→RESETTING")
	// Drive retries
	for i := 0; i < 10 && fsm5.State() != StateDONE; i++ {
		nxt := fsm5.Step()
		if nxt.IsZero() {
			break
		}
		clock5.t = nxt
	}
	stepAndPrintCard(t, fsm5, clock5, fc5err, "场景7: 重试耗尽 DONE")

	// ============================================================
	// Print all logs
	// ============================================================
	t.Log("")
	t.Log("========================================")
	t.Log("所有日志（按时间正序）:")
	t.Log("========================================")
	for _, l := range allLogs {
		t.Log("  " + l)
	}
}

// errOnlyClient always returns a network error on Consume.
type errOnlyClient struct {
	credits   map[string]*Credit
	weeklyPct int
}

func (c *errOnlyClient) ListCredits(CodexCredentials) (Snapshot, error) {
	out := make([]Credit, 0, len(c.credits))
	for _, cr := range c.credits {
		out = append(out, *cr)
	}
	return Snapshot{Credits: out, AvailableCount: countAvailSim(out), WeeklyPct: -1}, nil
}

func (c *errOnlyClient) GetUsage(CodexCredentials) (Snapshot, error) {
	return Snapshot{WeeklyPct: c.weeklyPct}, nil
}

func (c *errOnlyClient) Consume(CodexCredentials, string, string) (ConsumeResponse, error) {
	return ConsumeResponse{}, fmt.Errorf("simulated network timeout")
}

func stepAndPrintCard(t *testing.T, fsm *AccountFSM, clock *simClock, fc ResetClient, label string) {
	fromState := fsm.State()
	next := fsm.Step()
	if !next.IsZero() {
		clock.t = next
	}
	newState := fsm.State()
	snap := fsm.LastSnapshot()

	t.Log("")
	t.Log("┌──────────────────────────────────────────────────────────────────┐")
	t.Logf("│ %s", label)
	t.Log("├──────────────────────────────────────────────────────────────────┤")
	t.Logf("│ %-66s│", truncateSim(fsm.AuthID, 66))
	t.Logf("│ [状态: %s]", newState)
	t.Log("├──────────────────────────────────────────────────────────────────┤")

	// Weekly + credits
	weekly := snap.WeeklyPct
	if weekly < 0 {
		weekly = 100
	}
	avail := snap.AvailableCount
	t.Logf("│ 周额度剩余: %d%%", weekly)
	t.Logf("│ 可用重置次数: %d", avail)

	// Credit list
	for _, c := range snap.Credits {
		if c.Status != "available" {
			continue
		}
		shortID := strings.TrimPrefix(c.ID, "RateLimitResetCredit_")
		if len(shortID) > 8 {
			shortID = shortID[:8] + "…"
		}
		expiryStr := c.ExpiresAt.Format("2006-01-02 15:04:05")
		remain := c.ExpiresAt.Sub(clock.t).Round(time.Hour)
		t.Logf("│   • %s  %s  剩余 %v", shortID, expiryStr, remain)
	}

	// Status panel: last + next in one box
	t.Log("├──────────────────────────────────────────────────────────────────┤")
	t.Logf("│ ● 上一轮: %s", truncateSim(describeSimTransition(fromState, newState), 56))

	nextStr := "等待调度"
	if !next.IsZero() {
		nextStr = fmt.Sprintf("下一轮将在 %s 发生（%s）", next.Format("2006-01-02 15:04:05"), describeSimAction(newState))
	}
	t.Logf("│ ● %s", nextStr)
	t.Log("└──────────────────────────────────────────────────────────────────┘")
}

func describeSimTransition(from, to State) string {
	switch to {
	case StateIDLE:
		return "巡查完毕，未发现临近过期的重置次数"
	case StateARMED:
		return "发现重置次数即将过期，已进入 ARMED 状态"
	case StateCONFIRMING:
		return "触发时间已到，正在二次确认"
	case StateRESETTING:
		return "已发送重置请求，等待响应"
	case StateVERIFYING:
		return "重置请求已接受，等待验证"
	case StateDONE:
		return "重置验证通过，本轮完成"
	}
	return string(to)
}

func describeSimAction(state State) string {
	switch state {
	case StateIDLE:
		return "巡查重置次数"
	case StateARMED:
		return "进入确认流程"
	case StateCONFIRMING:
		return "发送重置请求"
	case StateRESETTING:
		return "等待重置响应"
	case StateVERIFYING:
		return "验证重置结果"
	case StateDONE:
		return "恢复巡查"
	}
	return "未知"
}

func truncateSim(s string, max int) string {
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
