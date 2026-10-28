package gateway

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Claude Code adds this local reply on native resume. Its model is
// <synthetic> in the transcript, but only role/content reach Messages.
const resumeLocalReply = "No response requested."

func TestClaudeSyntheticResumeKeepsTheRun(t *testing.T) {
	for _, shelved := range []bool{false, true} {
		for _, changedSchema := range []bool{false, true} {
			t.Run(fmt.Sprintf("shelved=%t/schemaChanged=%t", shelved, changedSchema), func(t *testing.T) {
				h := newResumeHarness(t)
				send := func(schema string, msgs ...string) {
					t.Helper()
					body := `{"model":"claude-sonnet-5","max_tokens":100,"system":"rules A","tools":[{"name":"read","input_schema":` + schema + `}],"messages":[` + strings.Join(msgs, ",") + `]}`
					var u Usage
					if code, msg := h.s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, h.p, "claude-sonnet-5", []byte(body), &u); code != 200 {
						t.Fatalf("%d %s", code, msg)
					}
				}
				schema := `{"type":"object"}`
				send(schema, user("original ask"))
				var pid, sid string
				for p := range h.runs() {
					pid, sid = p, "s"+p
				}
				if shelved {
					for i := range idleMost {
						h.ask(fmt.Sprintf("other rules %d", i), "other ask")
					}
					eventually(t, "the original session was not shelved", func() bool {
						h.s.subscription.mu.Lock()
						defer h.s.subscription.mu.Unlock()
						return len(h.s.subscription.shelf) == 1
					})
				}
				before := h.runs()
				if changedSchema {
					schema = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
				}
				send(schema, user("original ask"), assistant("ok"), assistant(resumeLocalReply), user("more after resume"))
				after := h.runs()
				var log string
				selectedPID := pid
				if !shelved && !changedSchema {
					log = after[pid]
					if len(after) != len(before) {
						t.Errorf("synthetic resume started a new run: %d -> %d", len(before), len(after))
						for p, l := range after {
							if _, had := before[p]; !had {
								selectedPID, log = p, l
							}
						}
					}
				} else {
					for p, l := range after {
						if _, had := before[p]; !had {
							selectedPID, log = p, l
						}
					}
					resumed := strings.Contains(log, "--resume "+sid)
					if resumed != (shelved && !changedSchema) {
						t.Errorf("resumed original session: %t; shelved=%t schemaChanged=%t\n%s", resumed, shelved, changedSchema, log)
					}
				}
				i := strings.LastIndex(log, "\ntold ")
				if i < 0 {
					t.Fatalf("no input sent to the selected run: %s", log)
				}
				told := log[i:]
				if !strings.Contains(told, resumeLocalReply) || !strings.Contains(told, "more after resume") {
					t.Errorf("resume lost local reply or new input: %s", told)
				}
				if changedSchema {
					var toolsPath string
					h.s.subscription.mu.Lock()
					for _, r := range h.s.subscription.idle {
						if fmt.Sprint(r.cmd.Process.Pid) == selectedPID {
							toolsPath = filepath.Join(r.tmp, "tools.json")
						}
					}
					h.s.subscription.mu.Unlock()
					tools, err := os.ReadFile(toolsPath)
					if err != nil || !strings.Contains(string(tools), `"required":["path"]`) {
						t.Errorf("new run was not given the changed schema: %s, %v", tools, err)
					}
					if len(after) != len(before)+1 || !strings.Contains(told, "original ask") {
						t.Errorf("schema change did not start a run with full history: %s", told)
					}
				} else if strings.Contains(told, "original ask") || strings.Contains(told, "conversation_history") {
					t.Errorf("synthetic resume rendered full history instead of the increment: %s", told)
				}
				if !changedSchema {
					send(schema, user("original ask"), assistant("ok"), assistant(resumeLocalReply), user("more after resume"), assistant("ok"), user("final ask"))
					last := h.runs()
					log = last[selectedPID]
					i = strings.LastIndex(log, "\ntold ")
					if len(last) != len(after) || i < 0 || !strings.Contains(log[i:], "final ask") || strings.Contains(log[i:], resumeLocalReply) || strings.Contains(log[i:], "original ask") {
						t.Errorf("turn after synthetic resume did not keep the run with only its new input: %v", last)
					}
				}
			})
		}
	}
}

func TestClaudeResumePrefixRequiresTheSameConversation(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*Request, *string)
		want   bool
	}{
		{"unchanged", func(*Request, *string) {}, true},
		{"other account", func(_ *Request, owner *string) { *owner = "other" }, false},
		{"other model", func(r *Request, _ *string) { r.Model = "other" }, false},
		{"other system", func(r *Request, _ *string) { r.System = "other" }, false},
		{"changed prefix", func(r *Request, _ *string) { r.Messages[0].Parts[0].Text = "other" }, false},
		{"another provider replied", func(r *Request, _ *string) { r.Messages[2].Parts[0].Text = "an answer from another provider" }, false},
		{"unmatched result", func(r *Request, _ *string) {
			r.Messages[2].Parts = []Part{{Kind: ToolResult, CallID: "unknown", Text: "result"}}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &Request{Model: "claude-sonnet-5", System: "rules", Messages: []Message{
				{Role: "user", Parts: []Part{{Kind: Text, Text: "original ask"}}},
				{Role: "assistant", Parts: []Part{{Kind: Text, Text: "ok"}}},
				{Role: "assistant", Parts: []Part{{Kind: Text, Text: resumeLocalReply}}},
				{Role: "user", Parts: []Part{{Kind: Text, Text: "more"}}},
			}}
			key := turnKey("owner", req, req.Messages[:2])
			b := newSubscriptionBridge()
			saved := &savedSession{key: key, timer: time.NewTimer(time.Hour)}
			t.Cleanup(func() { saved.timer.Stop() })
			b.shelf[key] = saved
			owner := "owner"
			c.change(req, &owner)
			got := b.unshelve(req, owner)
			if (got == saved) != c.want {
				t.Fatalf("found original conversation: %t, want %t", got == saved, c.want)
			}
			if c.want && (len(got.since) != 2 || got.since[0].Parts[0].Text != resumeLocalReply) {
				t.Fatalf("lost the local increment: %+v", got.since)
			}
		})
	}
}
