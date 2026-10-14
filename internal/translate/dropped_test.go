package translate

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SnowballSH/modelgate/internal/oai"
)

const everySamplingParam = `"temperature":0.7,"top_p":0.9,"frequency_penalty":0.5,"presence_penalty":0.5,"seed":7,"logprobs":true,"top_logprobs":3,"logit_bias":{"50256":-100},"stop":"END",`

var unsupportedNames = []string{"frequency_penalty", "presence_penalty", "seed", "logprobs", "top_logprobs", "logit_bias"}

func TestDroppedParamsPerUpstream(t *testing.T) {
	passthrough := func(dropSampling bool) func(oai.ChatRequest) []string {
		return func(req oai.ChatRequest) []string { return PassthroughDropped(req, dropSampling) }
	}
	cases := []struct {
		name    string
		fields  string
		dropped func(oai.ChatRequest) []string
		want    []string
	}{
		{"anthropic forwards sampling and stop", everySamplingParam, AnthropicOptions{}.Dropped, unsupportedNames},
		{"anthropic row without sampling", everySamplingParam, AnthropicOptions{DropSampling: true}.Dropped,
			append([]string{"temperature", "top_p"}, unsupportedNames...)},
		{"anthropic effort displaces sampling", `"reasoning_effort":"low","temperature":0.7,`, AnthropicOptions{}.Dropped, []string{"temperature"}},
		{"responses row drops sampling and stop", everySamplingParam, ResponsesOptions{DropSampling: true}.Dropped,
			append(append([]string{"temperature", "top_p"}, unsupportedNames...), "stop")},
		{"responses row with sampling", everySamplingParam, ResponsesOptions{}.Dropped, append(append([]string{}, unsupportedNames...), "stop")},
		{"passthrough forwards everything", everySamplingParam, passthrough(false), nil},
		{"passthrough row without sampling", everySamplingParam, passthrough(true), []string{"temperature", "top_p"}},
		{"neutral values are not drops", `"frequency_penalty":0,"presence_penalty":0,"logprobs":false,"top_logprobs":0,"logit_bias":{},"stop":"",`,
			ResponsesOptions{DropSampling: true}.Dropped, nil},
		{"nulls are not drops", `"temperature":null,"seed":null,"logit_bias":null,"stop":null,`, ResponsesOptions{DropSampling: true}.Dropped, nil},
		{"nothing sent", ``, AnthropicOptions{DropSampling: true}.Dropped, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := chatRequest(t, `{`+tc.fields+`"messages":[{"role":"user","content":"hi"}]}`)
			if got := tc.dropped(req); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dropped = %v, want %v", got, tc.want)
			}
		})
	}
}

func wireKeys(t *testing.T, v any, keys ...string) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, key := range keys {
		present[key] = bytes.Contains(raw, []byte(`"`+key+`"`))
	}
	return present
}

func TestToAnthropicDropsWhatItUsedToRefuse(t *testing.T) {
	req := chatRequest(t, `{`+everySamplingParam+`"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToAnthropicWith(req, AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024})
	if err != nil {
		t.Fatalf("a chat client's default parameters must translate: %v", err)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 || got.TopP == nil || len(got.StopSequences) != 1 {
		t.Errorf("sampling and stop must still be forwarded on a row that allows them: %+v", got)
	}
	for key, present := range wireKeys(t, got, "frequency_penalty", "presence_penalty", "seed", "logprobs", "top_logprobs", "logit_bias") {
		if present {
			t.Errorf("%s reached the anthropic upstream", key)
		}
	}
}

func TestToAnthropicRowWithoutSampling(t *testing.T) {
	req := chatRequest(t, `{"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToAnthropicWith(req, AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024, DropSampling: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature != nil || got.TopP != nil {
		t.Errorf("temperature %v and top_p %v reached a row declared sampling: false", got.Temperature, got.TopP)
	}
}

func TestToResponsesDropsWhatItUsedToRefuse(t *testing.T) {
	req := chatRequest(t, `{`+everySamplingParam+`"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToResponsesWith(req, ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1024, DropSampling: true})
	if err != nil {
		t.Fatalf("a chat client's default parameters must translate: %v", err)
	}
	for key, present := range wireKeys(t, got, "temperature", "top_p", "frequency_penalty", "presence_penalty", "seed", "logprobs", "top_logprobs", "logit_bias", "stop") {
		if present {
			t.Errorf("%s reached the responses upstream", key)
		}
	}
}

func TestToResponsesRowWithSamplingForwardsIt(t *testing.T) {
	req := chatRequest(t, `{"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToResponsesWith(req, ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 || got.TopP == nil || *got.TopP != 0.9 {
		t.Errorf("temperature %v, top_p %v; want both forwarded on a row declared sampling: true", got.Temperature, got.TopP)
	}
}
