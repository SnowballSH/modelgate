package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

const (
	tinyImage     = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	visionRowJSON = `{"models":{"claude-vision":{"provider_model":"claude-vision","image_input":true,
		"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":0.25,"cache_write_usd_per_mtok":20},
		"gpt-vision":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-vision","image_input":true,
		"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":0.25,"cache_write_usd_per_mtok":20}}}`
	inlineImagePrefix = "data:image/png;base64,"
)

func imageRequest(model string, stream bool, imageURL string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":1000,"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":%q}}]}]}`, model, stream, imageURL)
}

func TestImageOnARowWithoutImageInputIsRefusedByName(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "gpt-6-luna"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				env := newDualEnvWithTable(t, limitedModelJSON, dispatchOpenAI(), fullResponseHandler(), 100)
				auth, _ := insertTestKey(t, env.store, nil)

				rec := doDual(env, auth, imageRequest(model, stream, tinyImage))
				var body oai.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode %q: %v", rec.Body.String(), err)
				}
				want := fmt.Sprintf("model %s does not support image input", model)
				if rec.Code != http.StatusBadRequest || body.Error.Code != CodeInvalidRequest || body.Error.Message != want {
					t.Errorf("status %d, error %+v; want 400 %q", rec.Code, body.Error, want)
				}
				if *env.anthropicHits != 0 || len(*env.openaiCalls) != 0 {
					t.Errorf("upstream calls: anthropic %d, openai %v; want none", *env.anthropicHits, upstreamPaths(*env.openaiCalls))
				}
				if reserved := env.acct.Reserved(); reserved != 0 || len(env.guards.slots) != 0 {
					t.Errorf("a refused request holds reservation USD %v and %d slots, want none", reserved, len(env.guards.slots))
				}
				spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
				if err != nil {
					t.Fatal(err)
				}
				if spend != 0 {
					t.Errorf("spend = %v, want nothing booked for a refused request", spend)
				}
			})
		}
	}
}

func TestPassthroughRowForwardsImagesWhateverItDeclares(t *testing.T) {
	env := newDualEnvWithTable(t, limitedModelJSON, dispatchOpenAI(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	rec := doDual(env, auth, imageRequest("gpt-chat", false, tinyImage))
	if rec.Code != http.StatusOK || len(*env.openaiSeen) != 1 {
		t.Fatalf("status %d, upstream requests %d; a chat_completions row forwards content parts unchanged", rec.Code, len(*env.openaiSeen))
	}
}

func TestImagesReachBothTranslatedUpstreams(t *testing.T) {
	env, capture, auth := chatReadyEnv(t)

	if rec := doDual(env, auth, imageRequest("claude-chat", false, tinyImage)); rec.Code != http.StatusOK {
		t.Fatalf("claude-chat status = %d, body %s", rec.Code, rec.Body.String())
	}
	blocks := lastAnthropicRequest(t, capture).Messages[0].Content
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" {
		t.Errorf("anthropic blocks = %+v, want text then a base64 png image", blocks)
	}

	if rec := doDual(env, auth, imageRequest("gpt-vision", false, "https://example.com/cat.png")); rec.Code != http.StatusOK {
		t.Fatalf("gpt-vision status = %d, body %s", rec.Code, rec.Body.String())
	}
	parts := lastResponsesRequest(t, env).Input[0].Content
	if len(parts) != 2 || parts[1].Type != "input_image" || parts[1].ImageURL != "https://example.com/cat.png" {
		t.Errorf("responses parts = %+v, want text then the image url", parts)
	}
}

func TestLargeInlineImageReservesAnImageNotItsBytes(t *testing.T) {
	payload := strings.Repeat("A", 15<<20)
	body := imageRequest("claude-vision", false, inlineImagePrefix+payload)

	var env *publicEnv
	var reserved float64
	var upstreamImageBytes int
	answer := fullResponseHandler()
	env = newPublicEnvWith(t, func(w http.ResponseWriter, r *http.Request) {
		reserved = env.acct.Reserved()
		var areq anthro.MessagesRequest
		if json.NewDecoder(r.Body).Decode(&areq) == nil && len(areq.Messages) == 1 && len(areq.Messages[0].Content) == 2 && areq.Messages[0].Content[1].Source != nil {
			upstreamImageBytes = len(areq.Messages[0].Content[1].Source.Data)
		}
		answer(w, r)
	}, publicEnvOptions{maxBodyBytes: 16 << 20, budgetUSD: 100, rpm: 1000, tableJSON: visionRowJSON})
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
	}
	if upstreamImageBytes != len(payload) {
		t.Errorf("upstream image payload = %d bytes, want %d", upstreamImageBytes, len(payload))
	}
	textBytes := len(body) - len(payload)
	want := (float64(textBytes+anthropicImageTokenAllowance+promptOverheadTokens)*20 + 1000*50) / 1e6
	if math.Abs(reserved-want) > 1e-9 {
		t.Errorf("reservation while the image was in flight = USD %v, want %v: an image is %d tokens, not one per byte", reserved, want, anthropicImageTokenAllowance)
	}
}

func TestEachImageIsChargedItsUpstreamsAllowance(t *testing.T) {
	const payload = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tests := []struct {
		name      string
		model     string
		imageURL  string
		allowance int64
		textBytes func(body string) int
	}{
		{"anthropic https", "claude-vision", "https://example.com/cat.png", anthropicImageTokenAllowance, func(body string) int { return len(body) }},
		{"anthropic inline", "claude-vision", inlineImagePrefix + payload, anthropicImageTokenAllowance, func(body string) int { return len(body) - len(payload) }},
		{"responses https", "gpt-vision", "https://example.com/cat.png", openAIImageTokenAllowance, func(body string) int { return len(body) }},
		{"responses inline", "gpt-vision", inlineImagePrefix + payload, openAIImageTokenAllowance, func(body string) int { return len(body) - len(inlineImagePrefix) - len(payload) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var env *dualEnv
			var reserved float64
			answer, responses := fullResponseHandler(), openaiResponsesHandler()
			env = newDualEnvWithTable(t, visionRowJSON, func(w http.ResponseWriter, r *http.Request) {
				reserved = env.acct.Reserved()
				responses(w, r)
			}, func(w http.ResponseWriter, r *http.Request) {
				reserved = env.acct.Reserved()
				answer(w, r)
			}, 100)
			auth, _ := insertTestKey(t, env.store, nil)

			body := imageRequest(tc.model, false, tc.imageURL)
			if rec := doDual(env, auth, body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			want := (float64(int64(tc.textBytes(body))+tc.allowance+promptOverheadTokens)*20 + 1000*50) / 1e6
			if math.Abs(reserved-want) > 1e-9 {
				t.Errorf("reservation = USD %v, want %v: one image at %d tokens", reserved, want, tc.allowance)
			}
		})
	}
}

func TestImageAllowanceFollowsTheRowsUpstream(t *testing.T) {
	table := tableFrom(t, chatReadyModelJSON)
	tests := []struct {
		model string
		want  int64
	}{
		{"claude-chat", anthropicImageTokenAllowance},
		{"gpt-vision", openAIImageTokenAllowance},
		{"gpt-pass", openAIImageTokenAllowance},
	}
	for _, tc := range tests {
		row, ok := table.Resolve(tc.model)
		if !ok {
			t.Fatalf("%s is not in the table", tc.model)
		}
		if got := imageAllowance(row); got != tc.want {
			t.Errorf("%s: allowance = %d, want %d", tc.model, got, tc.want)
		}
	}
}

func TestCutStreamEstimateUsesTheRowsAllowance(t *testing.T) {
	table := tableFrom(t, visionRowJSON)
	for model, allowance := range map[string]int64{"claude-vision": anthropicImageTokenAllowance, "gpt-vision": openAIImageTokenAllowance} {
		row, _ := table.Resolve(model)
		prompt := measurePrompt(400, translate.ImageLoad{Count: 2}, imageAllowance(row))
		booked := streamMeter{started: true, prompt: prompt}.bookable(store.Usage{})
		if want := 100 + 2*allowance; booked.usage.InputTokens != want {
			t.Errorf("%s: input estimate = %d, want %d", model, booked.usage.InputTokens, want)
		}
	}
}

func TestCutStreamEstimateCountsImagesAsTokens(t *testing.T) {
	prompt := measurePrompt(15<<20+400, translate.ImageLoad{Count: 2, InlineBytes: 15 << 20}, anthropicImageTokenAllowance)
	booked := streamMeter{started: true, prompt: prompt}.bookable(store.Usage{})
	if want := int64(100 + 2*anthropicImageTokenAllowance); !booked.inputEstimated || booked.usage.InputTokens != want {
		t.Errorf("estimate = %+v, want %d input tokens: 400 text bytes at four per token plus two images", booked, want)
	}
}

func TestCutStreamEstimateCountsARemoteImage(t *testing.T) {
	booked := streamMeter{started: true, prompt: measurePrompt(400, translate.ImageLoad{Count: 1}, anthropicImageTokenAllowance)}.bookable(store.Usage{})
	if want := int64(100 + anthropicImageTokenAllowance); booked.usage.InputTokens != want {
		t.Errorf("input estimate = %d, want %d: 400 text bytes at four per token plus one remote image", booked.usage.InputTokens, want)
	}
}

func TestBookableDecidesWhetherInputWasEstimated(t *testing.T) {
	prompt := measurePrompt(400, translate.ImageLoad{}, anthropicImageTokenAllowance)
	tests := []struct {
		name     string
		meter    streamMeter
		reported store.Usage
		want     bool
	}{
		{"final usage reported", streamMeter{started: true, finalUsage: true, prompt: prompt}, store.Usage{InputTokens: 5}, false},
		{"stream never started", streamMeter{prompt: prompt}, store.Usage{}, false},
		{"cut before any input count", streamMeter{started: true, prompt: prompt}, store.Usage{OutputTokens: 3}, true},
		{"cut after the provider reported input", streamMeter{started: true, prompt: prompt}, store.Usage{InputTokens: 7}, false},
		{"cut after only cache reads were reported", streamMeter{started: true, prompt: prompt}, store.Usage{CacheReadTokens: 7}, false},
	}
	for _, tc := range tests {
		if got := tc.meter.bookable(tc.reported).inputEstimated; got != tc.want {
			t.Errorf("%s: inputEstimated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPromptSize(t *testing.T) {
	plain := measurePrompt(1000, translate.ImageLoad{}, anthropicImageTokenAllowance)
	if plain.worstCaseTokens() != 1000+promptOverheadTokens || plain.estimatedTokens() != 250 {
		t.Errorf("text-only prompt = %+v: worst case %d, estimate %d", plain, plain.worstCaseTokens(), plain.estimatedTokens())
	}
	remote := measurePrompt(1000, translate.ImageLoad{Count: 1}, anthropicImageTokenAllowance)
	if remote.worstCaseTokens() != 1000+anthropicImageTokenAllowance+promptOverheadTokens {
		t.Errorf("a remote image must add its allowance to the bound, got %d", remote.worstCaseTokens())
	}
}

const alphabetRefusal = "image_url.url data URI payload must use only the standard base64 alphabet (A-Z, a-z, 0-9, + and /, with = only at the end)"

func rawImageBody(model, text, imageURL string) string {
	return `{"model":"` + model + `","max_tokens":1000,"messages":[{"role":"user","content":[{"type":"text","text":"` + text + `"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`
}

type reservationProbe struct {
	env      *dualEnv
	auth     string
	reserved *float64
}

func newReservationProbe(t *testing.T) reservationProbe {
	t.Helper()
	var probe reservationProbe
	var reserved float64
	answer, responses := fullResponseHandler(), openaiResponsesHandler()
	probe.env = newDualEnvWithTable(t, visionRowJSON, func(w http.ResponseWriter, r *http.Request) {
		reserved = probe.env.acct.Reserved()
		responses(w, r)
	}, func(w http.ResponseWriter, r *http.Request) {
		reserved = probe.env.acct.Reserved()
		answer(w, r)
	}, 100)
	probe.auth, _ = insertTestKey(t, probe.env.store, nil)
	probe.reserved = &reserved
	return probe
}

func (p reservationProbe) textOnlyBound(text string, allowance int64) float64 {
	return (float64(int64(len(text))+allowance+promptOverheadTokens)*20 + 1000*50) / 1e6
}

func TestRawInvalidUTF8InAPayloadIsRefusedAndReservesNothing(t *testing.T) {
	shapes := []struct {
		name    string
		text    int
		invalid int
	}{
		{"large text, 200k invalid bytes", 400_000, 200_000},
		{"small text, 300k invalid bytes", 1_000, 300_000},
	}
	for _, model := range []string{"claude-vision", "gpt-vision"} {
		for _, shape := range shapes {
			t.Run(model+"/"+shape.name, func(t *testing.T) {
				probe := newReservationProbe(t)
				payload := append([]byte("AAAA"), bytes.Repeat([]byte{0xFF}, shape.invalid)...)
				body := rawImageBody(model, strings.Repeat("t", shape.text), inlineImagePrefix+string(payload))

				rec := doDual(probe.env, probe.auth, body)
				var refusal oai.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode %.200q: %v", rec.Body.String(), err)
				}
				if rec.Code != http.StatusBadRequest || refusal.Error.Message != alphabetRefusal {
					t.Errorf("status %d, message %q; want 400 %q", rec.Code, refusal.Error.Message, alphabetRefusal)
				}
				if *probe.reserved != 0 || probe.env.acct.Reserved() != 0 || len(probe.env.guards.slots) != 0 || *probe.env.anthropicHits+len(*probe.env.openaiCalls) != 0 {
					t.Errorf("a refused request reserved USD %v / held %d slots / reached an upstream", probe.env.acct.Reserved(), len(probe.env.guards.slots))
				}
			})
		}
	}
}

func TestEscapedAndDuplicatedImageKeysNeverReserveLessThanTheText(t *testing.T) {
	text := strings.Repeat("t", 200_000)
	escaped := inlineImagePrefix + "AAAA" + strings.Repeat(`\/`, 40_000) + strings.Repeat(`\u0041`, 10_000)
	duplicated := func(model string) string {
		big := inlineImagePrefix + strings.Repeat("A", 100_000)
		return `{"model":"` + model + `","max_tokens":1000,"messages":[{"role":"user","content":"ignored","content":[{"type":"text","text":"` + text + `"},` +
			`{"type":"image_url","image_url":{"url":"` + big + `"},"image_url":{"url":"https://example.com/cat.png"}}]}]}`
	}
	tests := []struct {
		name string
		body func(model string) string
	}{
		{"json escapes in the payload", func(model string) string { return rawImageBody(model, text, escaped) }},
		{"duplicate content and image_url keys", duplicated},
	}
	for model, allowance := range map[string]int64{"claude-vision": anthropicImageTokenAllowance, "gpt-vision": openAIImageTokenAllowance} {
		for _, tc := range tests {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				probe := newReservationProbe(t)
				rec := doDual(probe.env, probe.auth, tc.body(model))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
				}
				if floor := probe.textOnlyBound(text, allowance); *probe.reserved < floor-1e-9 {
					t.Errorf("reservation = USD %v, below the text plus one image allowance %v", *probe.reserved, floor)
				}
			})
		}
	}
}

func TestWellFormedImageReservesTextPlusTheAllowance(t *testing.T) {
	text := strings.Repeat("t", 50_000)
	payload := strings.Repeat("A", 100_000)
	for model, allowance := range map[string]int64{"claude-vision": anthropicImageTokenAllowance, "gpt-vision": openAIImageTokenAllowance} {
		t.Run(model, func(t *testing.T) {
			probe := newReservationProbe(t)
			if rec := doDual(probe.env, probe.auth, rawImageBody(model, text, inlineImagePrefix+payload)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
			}
			overhead := 0
			if model == "claude-vision" {
				overhead = len(inlineImagePrefix)
			}
			jsonOverhead := len(rawImageBody(model, "", ""))
			want := (float64(int64(len(text)+jsonOverhead+overhead)+allowance+promptOverheadTokens)*20 + 1000*50) / 1e6
			if math.Abs(*probe.reserved-want) > 1e-9 {
				t.Errorf("reservation = USD %v, want %v", *probe.reserved, want)
			}
		})
	}
}

func decodeErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %.200q: %v", rec.Body.String(), err)
	}
	return body.Error.Message
}

func passthroughChatBody(content string) string {
	return `{"model":"gpt-pass","max_tokens":1000,"messages":[{"role":"user","content":` + content + `}]}`
}

func passthroughReservation(t *testing.T, body string) (reserved float64, rec *httptest.ResponseRecorder, env *dualEnv) {
	t.Helper()
	chat := openaiChatResponse()
	env = newDualEnvWithTable(t, chatReadyModelJSON, func(w http.ResponseWriter, r *http.Request) {
		reserved = env.acct.Reserved()
		chat(w, r)
	}, fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	return reserved, doDual(env, auth, body), env
}

func TestPassthroughImageCountCannotBeShadowedOrZeroed(t *testing.T) {
	const image = `"image_url":{"url":"https://example.com/a.png"}`
	tests := []struct {
		name    string
		content string
		images  int64
		refused bool
	}{
		{"plain image", `[{"type":"image_url",` + image + `}]`, 1, false},
		{"inline image with detail inside image_url", `[{"type":"image_url","image_url":{"url":"` + tinyImage + `","detail":"low"}}]`, 1, false},
		{"two images beside text", `[{"type":"text","text":"hi"},{"type":"image_url",` + image + `},{"type":"image_url",` + image + `}]`, 2, false},
		{"image with a null image_url", `[{"type":"image_url","image_url":null}]`, 1, false},
		{"plain text", `[{"type":"text","text":"hi"}]`, 0, false},
		{"text with cache_control", `[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]`, 0, false},
		{"number beyond float64 range in a value", `[{"type":"text","text":1e400}]`, 0, false},
		{"refusal part", `[{"type":"refusal","refusal":"no"}]`, 0, false},
		{"text and refusal parts", `[{"type":"text","text":"a"},{"type":"refusal","refusal":"no"}]`, 0, false},
		{"empty array", `[]`, 0, false},
		{"shadowing Type key", `[{"type":"image_url","Type":"text",` + image + `}]`, 0, true},
		{"shadowing TYPE key", `[{"type":"image_url","TYPE":"text",` + image + `}]`, 0, true},
		{"escaped shadowing key", `[{"type":"image_url","\u0054YPE":"text",` + image + `}]`, 0, true},
		{"duplicate type, last is text", `[{"type":"image_url","type":"text",` + image + `}]`, 0, true},
		{"duplicate type, first is text", `[{"type":"text","type":"image_url",` + image + `}]`, 0, true},
		{"differently cased type only", `[{"Type":"image_url",` + image + `}]`, 0, true},
		{"escaped type key", `[{"\u0074ype":"text"}]`, 0, true},
		{"type value TEXT", `[{"type":"TEXT","text":"hi"}]`, 0, true},
		{"type value Image_Url", `[{"type":"Image_Url",` + image + `}]`, 0, true},
		{"escaped type value", `[{"type":"image\u005furl",` + image + `}]`, 0, true},
		{"duplicate text key", `[{"type":"text","text":"a","text":"b"}]`, 0, true},
		{"duplicate image_url key", `[{"type":"image_url",` + image + `,` + image + `}]`, 0, true},
		{"re-cased Text key", `[{"type":"text","Text":"hi"}]`, 0, true},
		{"image_url key without type", `[{` + image + `}]`, 0, true},
		{"null image_url beside text", `[{"type":"text","text":"hi","image_url":null}]`, 0, true},
		{"string image_url on a text part", `[{"type":"text","image_url":"https://example.com/a.png"}]`, 0, true},
		{"number beyond float64 range as a key value", `[{"type":"text","text":"x","n":1e400}]`, 0, true},
		{"part without a type key", `[{"text":"hi"}]`, 0, true},
		{"empty object part", `[{}]`, 0, true},
		{"file part without a type", `[{"file":{"file_id":"f"}}]`, 0, true},
		{"input_audio part without a type", `[{"input_audio":{"data":"AAAA","format":"wav"}}]`, 0, true},
		{"file_id part without a type", `[{"file_id":"f"}]`, 0, true},
		{"text carrying a file", `[{"type":"text","file":{"file_id":"f"}}]`, 0, true},
		{"text carrying FILE", `[{"type":"text","FILE":{"file_id":"f"}}]`, 0, true},
		{"text carrying file_id", `[{"type":"text","file_id":"f"}]`, 0, true},
		{"text carrying input_audio", `[{"type":"text","input_audio":{"data":"AAAA","format":"wav"}}]`, 0, true},
		{"text carrying audio", `[{"type":"text","audio":{"id":"a"}}]`, 0, true},
		{"text carrying video_url", `[{"type":"text","video_url":{"url":"https://example.com/v.mp4"}}]`, 0, true},
		{"text carrying input_file", `[{"type":"text","input_file":"f"}]`, 0, true},
		{"text carrying a null file", `[{"type":"text","text":"hi","file":null}]`, 0, true},
		{"text carrying an unknown key", `[{"type":"text","x":1}]`, 0, true},
		{"image carrying text", `[{"type":"image_url",` + image + `,"text":"hi"}]`, 0, true},
		{"image carrying cache_control", `[{"type":"image_url",` + image + `,"cache_control":{}}]`, 0, true},
		{"refusal carrying text", `[{"type":"refusal","refusal":"no","text":"hi"}]`, 0, true},
		{"file part", `[{"type":"file","file":{"file_id":"f"}}]`, 0, true},
		{"input_audio part", `[{"type":"text","text":"hi"},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]`, 0, true},
		{"invented type", `[{"type":"hologram"}]`, 0, true},
		{"non-string Type key", `[{"type":"image_url","Type":5,` + image + `}]`, 0, true},
		{"sibling with a non-string type", `[{"type":"image_url",` + image + `},{"type":5}]`, 0, true},
		{"bare string element", `[{"type":"image_url",` + image + `},"x"]`, 0, true},
		{"nested array", `[[{"type":"image_url",` + image + `}]]`, 0, true},
		{"object content", `{"type":"image_url",` + image + `}`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := passthroughChatBody(tc.content)
			reserved, rec, env := passthroughReservation(t, body)
			if tc.refused {
				if rec.Code != http.StatusBadRequest || !strings.HasPrefix(decodeErrorMessage(t, rec), "invalid message content: ") {
					t.Errorf("status %d, body %s; want a 400 invalid message content", rec.Code, rec.Body.String())
				}
				if reserved != 0 || env.acct.Reserved() != 0 || len(*env.openaiCalls) != 0 {
					t.Errorf("a refused request reserved USD %v or reached the upstream", reserved)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
			}
			want := (float64(int64(len(body))+tc.images*openAIImageTokenAllowance+promptOverheadTokens) + 1000) / 1e6
			if math.Abs(reserved-want) > 1e-9 {
				t.Errorf("reservation = USD %v, want exactly %v: %d image(s) at %d tokens each, bytes kept as text", reserved, want, tc.images, openAIImageTokenAllowance)
			}
		})
	}
}

func TestPassthroughStringAndNullContentAreUntouched(t *testing.T) {
	for _, content := range []string{`"hello"`, `null`} {
		if _, rec, _ := passthroughReservation(t, passthroughChatBody(content)); rec.Code != http.StatusOK {
			t.Errorf("content %s: status = %d, body %.200s", content, rec.Code, rec.Body.String())
		}
	}
}

func TestPassthroughToolConversationIsAdmittedWithExactReservation(t *testing.T) {
	body := `{"model":"gpt-pass","max_tokens":1000,"messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"refusal","refusal":"no"}]},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"w","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"sunny"}]}]}`
	reserved, rec, _ := passthroughReservation(t, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
	}
	want := (float64(int64(len(body))+promptOverheadTokens) + 1000) / 1e6
	if math.Abs(reserved-want) > 1e-9 {
		t.Errorf("reservation = USD %v, want exactly %v", reserved, want)
	}
}

func TestPassthroughCutStreamEstimateChargesEachImage(t *testing.T) {
	truncated := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-pass\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"twelve chars\"},\"finish_reason\":null}]}\n\n")
	}
	env := newDualEnvWithTable(t, chatReadyModelJSON, truncated, fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	body := `{"model":"gpt-pass","stream":true,"max_tokens":1000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},` +
		`{"type":"image_url","image_url":{"url":"` + inlineImagePrefix + strings.Repeat("A", 4000) + `"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	doDual(env, auth, body)

	spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	want := (float64((len(body)+3)/4) + 2*openAIImageTokenAllowance + 3) / 1e6
	if math.Abs(spend-want) > 1e-12 {
		t.Errorf("cut stream booked USD %v, want %v: the body's bytes as text plus two images at %d tokens", spend, want, openAIImageTokenAllowance)
	}
}

func TestMeasurePromptFloorsTheTextRemainderAtZero(t *testing.T) {
	prompt := measurePrompt(100, translate.ImageLoad{Count: 1, InlineBytes: 5000}, anthropicImageTokenAllowance)
	if prompt.textBytes != 0 || prompt.worstCaseTokens() != anthropicImageTokenAllowance+promptOverheadTokens {
		t.Errorf("prompt = %+v: an inline count above the body must leave no negative text", prompt)
	}
}

func TestStrayImageURLKeyOnATextPartIsServedAsTextOnTranslatedRows(t *testing.T) {
	const textPart = `{"type":"text","text":"hi"`
	rates := map[string]float64{"claude-plain": 2.5, "claude-chat": 4}
	for _, model := range []string{"claude-plain", "claude-chat"} {
		for name, stray := range map[string]string{
			"null image_url":         `"image_url":null`,
			"re-cased image_url key": `"Image_URL":{"url":"https://example.com/a.png"}`,
		} {
			t.Run(model+"/"+name, func(t *testing.T) {
				var env *dualEnv
				var reserved float64
				capture := &upstreamCapture{}
				answer := capture.anthropic()
				env = newDualEnvWithTable(t, chatReadyModelJSON, dispatchOpenAI(), func(w http.ResponseWriter, r *http.Request) {
					reserved = env.acct.Reserved()
					answer(w, r)
				}, 100)
				auth, _ := insertTestKey(t, env.store, nil)

				body := `{"model":"` + model + `","max_tokens":1000,"messages":[{"role":"user","content":[` + textPart + `,` + stray + `}]}]}`
				rec := doDual(env, auth, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
				}
				blocks := lastAnthropicRequest(t, capture).Messages[0].Content
				if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "hi" {
					t.Errorf("anthropic blocks = %+v, want the one text block", blocks)
				}
				want := (float64(len(body)+promptOverheadTokens)*rates[model] + 1000*10) / 1e6
				if math.Abs(reserved-want) > 1e-9 {
					t.Errorf("reservation = USD %v, want %v: no image was sent, so no allowance", reserved, want)
				}
			})
		}
	}
}

func TestImageBesideAnUnusualButValidPartIsRefusedByModelName(t *testing.T) {
	env, _, auth := chatReadyEnv(t)
	body := `{"model":"claude-plain","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"text","text":"x","n":1e400}]}]}`
	rec := doDual(env, auth, body)
	if want := "model claude-plain does not support image input"; rec.Code != http.StatusBadRequest || decodeErrorMessage(t, rec) != want {
		t.Errorf("status %d, message %q; want 400 %q", rec.Code, decodeErrorMessage(t, rec), want)
	}
}
