package gateway

import "testing"

// The 11-09 Fable incident had consecutive tool rounds; the request body
// was not retained. This constructs that shape with a split earlier reply.
func TestClaudeFableSplitHistoryFollowsDuringToolContinuation(t *testing.T) {
	parse := func(body string) []Message {
		t.Helper()
		req, err := parseAnthropic([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return req.Messages
	}
	heard := parse(`{"model":"claude-fable-5-1","max_tokens":512,"messages":[
		{"role":"user","content":"check the result"},
		{"role":"assistant","content":[{"type":"text","text":"checked:done"},{"type":"tool_use","id":"tool_1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"ok"}]}
	]}`)
	next := parse(`{"model":"claude-fable-5-1","max_tokens":512,"messages":[
		{"role":"user","content":"check the result"},
		{"role":"assistant","content":[{"type":"text","text":"checked:"},{"type":"thinking","thinking":"reasoning","signature":"sig"},{"type":"text","text":"done"},{"type":"tool_use","id":"tool_1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"ok"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"tool_2","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_2","content":"ok"}]}
	]}`)
	run := &subscriptionRun{}
	run.heard(heard)
	if !run.follows(next) {
		t.Fatal("unchanged history split into text blocks must continue the waiting tool run")
	}
	next[1].Parts[0].Text = "changed:"
	if run.follows(next) {
		t.Fatal("changed history must not continue the waiting tool run")
	}
}
