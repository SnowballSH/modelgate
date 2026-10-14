package translate

import (
	"strings"
	"testing"
)

const emptyTurnHistory = `{"messages":[
	{"role":"user","content":"first"},
	{"role":"assistant","content":%s},
	{"role":"user","content":"second"}]}`

var emptyAssistantContents = map[string]string{
	"empty string":  `""`,
	"null":          `null`,
	"whitespace":    `" \n\t"`,
	"empty parts":   `[]`,
	"blank text":    `[{"type":"text","text":""}]`,
	"spaces in one": `[{"type":"text","text":"  "}]`,
}

func TestEmptyAssistantTurnsAreSkipped(t *testing.T) {
	for name, content := range emptyAssistantContents {
		t.Run(name, func(t *testing.T) {
			req := chatRequest(t, strings.Replace(emptyTurnHistory, "%s", content, 1))

			areq, err := ToAnthropic(req, "claude-real", 1024)
			if err != nil {
				t.Fatalf("ToAnthropic: %v", err)
			}
			if len(areq.Messages) != 2 || areq.Messages[0].Role != "user" || areq.Messages[1].Role != "user" {
				t.Errorf("anthropic messages = %+v, want the two user turns", areq.Messages)
			}

			rreq, err := ToResponses(req, "gpt-real", 1024)
			if err != nil {
				t.Fatalf("ToResponses: %v", err)
			}
			if len(rreq.Input) != 2 || rreq.Input[0].Role != "user" || rreq.Input[1].Role != "user" {
				t.Errorf("responses input = %+v, want the two user turns", rreq.Input)
			}
		})
	}
}

func TestWhitespaceAssistantTextIsNotSent(t *testing.T) {
	req := chatRequest(t, `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"\n","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"18C"}]}`)

	areq, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	assistant := areq.Messages[1]
	if len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" {
		t.Errorf("assistant blocks = %+v, want the tool_use alone: Anthropic refuses a whitespace-only text block", assistant.Content)
	}

	rreq, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(rreq.Input) != 3 || rreq.Input[1].Type != "function_call" {
		t.Errorf("responses input = %+v, want user, function_call, function_call_output", rreq.Input)
	}
}

func TestAssistantTextAroundWhitespaceIsSentUnchanged(t *testing.T) {
	req := chatRequest(t, `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":" hello \n"},
		{"role":"user","content":"again"}]}`)

	areq, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got := areq.Messages[1].Content[0].Text; got != " hello \n" {
		t.Errorf("anthropic assistant text = %q", got)
	}

	rreq, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got := rreq.Input[1].Text; got != " hello \n" {
		t.Errorf("responses assistant text = %q", got)
	}
}

func TestEmptyAssistantTurnsAmongRealOnesLeaveTheRealOnesInOrder(t *testing.T) {
	req := chatRequest(t, `{"messages":[
		{"role":"user","content":"one"},
		{"role":"assistant","content":""},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"two"},
		{"role":"assistant","content":null}]}`)

	areq, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range areq.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,user" {
		t.Errorf("anthropic roles = %s, want user,assistant,user", got)
	}

	rreq, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(rreq.Input) != 3 || rreq.Input[1].Text != "answer" {
		t.Errorf("responses input = %+v, want user, answer, user", rreq.Input)
	}
}

func TestAConversationOfOnlyEmptyAssistantTurnsHasNoMessages(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"system","content":"Be brief."},{"role":"assistant","content":""}]}`)
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Errorf("anthropic err = %v, want the no-messages refusal", err)
	}
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Errorf("responses err = %v, want the no-messages refusal", err)
	}
}

func TestAnEmptyUserMessageIsStillRefused(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"user","content":""}]}`)
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || !strings.Contains(err.Error(), "message content is empty") {
		t.Errorf("anthropic err = %v", err)
	}
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || !strings.Contains(err.Error(), "message content is empty") {
		t.Errorf("responses err = %v", err)
	}
}

func TestAnEmptyUserMessageBesideAnEmptyAssistantTurnIsStillRefused(t *testing.T) {
	req := chatRequest(t, `{"messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":""},
		{"role":"user","content":"  "},
		{"role":"user","content":""}]}`)
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || !strings.Contains(err.Error(), "message content is empty") {
		t.Errorf("anthropic err = %v", err)
	}
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || !strings.Contains(err.Error(), "message content is empty") {
		t.Errorf("responses err = %v", err)
	}
}
