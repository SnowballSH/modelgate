package translate

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

const (
	pngBase64  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	pngDataURI = "data:image/png;base64," + pngBase64
	visionTurn = `{"messages":[{"role":"user","content":[
		{"type":"text","text":"What is in these?"},
		{"type":"image_url","image_url":{"url":"` + pngDataURI + `","detail":"low"}},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.webp"}}]}]}`
)

var (
	visionAnthropic = AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024, ImageInput: true}
	visionResponses = ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1024, DropSampling: true, ImageInput: true}
)

func wantMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	return want
}

func TestImagePartsTranslateForBothUpstreams(t *testing.T) {
	req := chatRequest(t, visionTurn)

	areq, err := ToAnthropicWith(req, visionAnthropic)
	if err != nil {
		t.Fatalf("ToAnthropicWith: %v", err)
	}
	if diff := cmpMaps(asMap(t, areq.Messages[0]), wantMap(t, `{"role":"user","content":[
		{"type":"text","text":"What is in these?"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+pngBase64+`"}},
		{"type":"image","source":{"type":"url","url":"https://example.com/cat.webp"}}]}`)); diff != "" {
		t.Error(diff)
	}

	rreq, err := ToResponsesWith(req, visionResponses)
	if err != nil {
		t.Fatalf("ToResponsesWith: %v", err)
	}
	if diff := cmpMaps(asMap(t, rreq.Input[0]), wantMap(t, `{"type":"message","role":"user","content":[
		{"type":"input_text","text":"What is in these?"},
		{"type":"input_image","image_url":"`+pngDataURI+`","detail":"low"},
		{"type":"input_image","image_url":"https://example.com/cat.webp"}]}`)); diff != "" {
		t.Error(diff)
	}
}

func TestImageOnlyUserMessageIsNotEmpty(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"user","content":[
		{"type":"text","text":""},
		{"type":"image_url","image_url":{"url":"`+pngDataURI+`"}}]}]}`)
	areq, err := ToAnthropicWith(req, visionAnthropic)
	if err != nil {
		t.Fatalf("an image with no caption is a message: %v", err)
	}
	if blocks := areq.Messages[0].Content; len(blocks) != 1 || blocks[0].Type != "image" {
		t.Errorf("blocks = %+v, want the image alone (an empty text block is refused upstream)", blocks)
	}
	if _, err := ToResponsesWith(req, visionResponses); err != nil {
		t.Fatalf("ToResponsesWith: %v", err)
	}
}

func TestUnusableImagesAreRefused(t *testing.T) {
	const alphabetMessage = "image_url.url data URI payload must use only the standard base64 alphabet (A-Z, a-z, 0-9, + and /, with = only at the end)"
	const mediaTypes = "use one of image/jpeg, image/png, image/gif, image/webp"
	cases := []struct {
		name    string
		part    string
		message string
	}{
		{"plain http", `{"url":"http://example.com/cat.png"}`, "image_url.url must be a data: URI or an https URL"},
		{"file scheme", `{"url":"file:///etc/passwd"}`, "image_url.url must be a data: URI or an https URL"},
		{"no url", `{}`, "image_url.url must be a data: URI or an https URL"},
		{"bitmap", `{"url":"data:image/bmp;base64,Qk0="}`, `unsupported image media type "image/bmp"; ` + mediaTypes},
		{"svg", `{"url":"data:image/svg+xml;base64,PHN2Zz4="}`, `unsupported image media type "image/svg+xml"; ` + mediaTypes},
		{"not base64", `{"url":"data:image/png,rawbytes"}`, "image_url.url data URI must carry base64 data"},
		{"no payload", `{"url":"data:image/png;base64,"}`, "image_url.url data URI must carry base64 data"},
		{"url-safe alphabet", `{"url":"data:image/png;base64,AA-_AAAA"}`, alphabetMessage},
		{"embedded whitespace", `{"url":"data:image/png;base64,AAAA AAAA"}`, alphabetMessage},
		{"embedded newline escape", `{"url":"data:image/png;base64,AAAA\nAAAA"}`, alphabetMessage},
		{"padding before the end", `{"url":"data:image/png;base64,AA=AAAAA"}`, alphabetMessage},
		{"more than two padding characters", `{"url":"data:image/png;base64,AAAAA==="}`, alphabetMessage},
		{"non-ascii", `{"url":"data:image/png;base64,AAAA\u00ffAAAA"}`, alphabetMessage},
		{"only padding", `{"url":"data:image/png;base64,===="}`, alphabetMessage},
		{"length not a multiple of four", `{"url":"data:image/png;base64,AAAAA"}`, alphabetMessage},
		{"unknown detail", `{"url":"` + pngDataURI + `","detail":"ultra"}`, `image_url.detail "ultra" is not one of auto, low, high`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":`+tc.part+`}]}]}`)
			if _, err := ToAnthropicWith(req, visionAnthropic); err == nil || err.Error() != tc.message {
				t.Errorf("anthropic err = %v, want %q", err, tc.message)
			}
			if _, err := ToResponsesWith(req, visionResponses); err == nil || err.Error() != tc.message {
				t.Errorf("responses err = %v, want %q", err, tc.message)
			}
		})
	}
}

func TestMediaTypeMustBeExactlyLowercase(t *testing.T) {
	const message = `unsupported image media type "IMAGE/JPEG"; use one of image/jpeg, image/png, image/gif, image/webp`
	req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:IMAGE/JPEG;base64,`+pngBase64+`"}}]}]}`)
	if _, err := ToAnthropicWith(req, visionAnthropic); err == nil || err.Error() != message {
		t.Errorf("anthropic err = %v, want %q", err, message)
	}
	if _, err := ToResponsesWith(req, visionResponses); err == nil || err.Error() != message {
		t.Errorf("responses err = %v, want %q", err, message)
	}
}

func TestEchoedClientStringsAreTruncated(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	cases := []struct {
		name string
		part string
		echo string
	}{
		{"detail", `{"url":"` + pngDataURI + `","detail":"` + long + `"}`, long},
		{"media type", `{"url":"data:` + long + `;base64,AAAA"}`, long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":`+tc.part+`}]}]}`)
			for name, err := range map[string]error{
				"anthropic": func() error { _, err := ToAnthropicWith(req, visionAnthropic); return err }(),
				"responses": func() error { _, err := ToResponsesWith(req, visionResponses); return err }(),
			} {
				if err == nil || len(err.Error()) > 200 || !strings.Contains(err.Error(), strings.Repeat("x", 16)) {
					t.Errorf("%s: err = %.120v (%d bytes), want a refusal echoing a short prefix of the value", name, err, len(err.Error()))
				}
			}
		})
	}
}

func TestBorrowedJSONCannotBeAppendedOver(t *testing.T) {
	const content = `{"url":"a"},"second":2}`
	buffer := []byte(content)
	var borrowed borrowedJSON
	if err := borrowed.UnmarshalJSON(buffer[:len(`{"url":"a"}`)]); err != nil {
		t.Fatal(err)
	}
	grown := append(borrowed, '!')
	if len(grown) != len(borrowed)+1 || string(buffer) != content {
		t.Errorf("appending to the borrowed bytes rewrote the content buffer: %s", buffer)
	}
}

func TestImagesNeedTheRowToAllowThem(t *testing.T) {
	req := chatRequest(t, visionTurn)
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || err.Error() != "unsupported content part: image_url" {
		t.Errorf("anthropic err = %v", err)
	}
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || err.Error() != "unsupported content part: image_url" {
		t.Errorf("responses err = %v", err)
	}
}

func TestImagesAreUserContentOnly(t *testing.T) {
	image := `[{"type":"image_url","image_url":{"url":"` + pngDataURI + `"}}]`
	for name, messages := range map[string]string{
		"system":    `[{"role":"system","content":` + image + `},{"role":"user","content":"hi"}]`,
		"assistant": `[{"role":"user","content":"hi"},{"role":"assistant","content":` + image + `}]`,
		"tool": `[{"role":"user","content":"hi"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":` + image + `}]`,
	} {
		t.Run(name, func(t *testing.T) {
			req := chatRequest(t, `{"messages":`+messages+`}`)
			if _, err := ToAnthropicWith(req, visionAnthropic); err == nil || err.Error() != "unsupported content part: image_url" {
				t.Errorf("anthropic err = %v", err)
			}
			if _, err := ToResponsesWith(req, visionResponses); err == nil || err.Error() != "unsupported content part: image_url" {
				t.Errorf("responses err = %v", err)
			}
		})
	}
}

func TestAudioAndFilePartsStayRefused(t *testing.T) {
	for _, partType := range []string{"input_audio", "file"} {
		req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"`+partType+`"}]}]}`)
		want := "unsupported content part: " + partType
		if _, err := ToAnthropicWith(req, visionAnthropic); err == nil || err.Error() != want {
			t.Errorf("anthropic err = %v, want %q", err, want)
		}
		if _, err := ToResponsesWith(req, visionResponses); err == nil || err.Error() != want {
			t.Errorf("responses err = %v, want %q", err, want)
		}
	}
}

func TestImageLoadCountsImagesAndInlineBytes(t *testing.T) {
	req := chatRequest(t, visionTurn)
	areq, err := ToAnthropicWith(req, visionAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := AnthropicImageLoad(areq), (ImageLoad{Count: 2, InlineBytes: len(pngBase64)}); got != want {
		t.Errorf("AnthropicImageLoad = %+v, want %+v", got, want)
	}
	rreq, err := ToResponsesWith(req, visionResponses)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ResponsesImageLoad(rreq), (ImageLoad{Count: 2, InlineBytes: len(pngDataURI)}); got != want {
		t.Errorf("ResponsesImageLoad = %+v, want %+v", got, want)
	}
	if got := AnthropicImageLoad(anthro.MessagesRequest{}); got != (ImageLoad{}) {
		t.Errorf("empty request load = %+v", got)
	}
	if got := ResponsesImageLoad(oairesp.Request{}); got != (ImageLoad{}) {
		t.Errorf("empty request load = %+v", got)
	}
}

func TestHasImageParts(t *testing.T) {
	cases := map[string]bool{
		visionTurn: true,
		`{"messages":[{"role":"user","content":"a string mentioning image_url"}]}`:                             false,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`:                               false,
		`{"messages":[{"role":"system","content":[{"type":"image_url"}]},{"role":"user","content":"x"}]}`:      true,
		`{"messages":[{"role":"assistant"}]}`:                                                                  false,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","image_url":null}]}]}`:              false,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","Image_URL":{"url":"x"}}]}]}`:       false,
		`{"messages":[{"role":"user","content":[{"type":"image_url"},{"type":"text","text":"x","n":1e400}]}]}`: true,
		`{"messages":[{"role":"user","content":[{"Type":"image_url"}]}]}`:                                      true,
	}
	for body, want := range cases {
		if got := HasImageParts(chatRequest(t, body).Messages); got != want {
			t.Errorf("HasImageParts(%.60s…) = %v, want %v", body, got, want)
		}
	}
}

func TestInlineImageIsTranslatedWithOneCopy(t *testing.T) {
	payload := strings.Repeat("A", 15<<20)
	req := chatRequest(t, `{"messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,`+payload+`"}}]}]}`)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	areq, err := ToAnthropicWith(req, visionAnthropic)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(areq.Messages[0].Content[1].Source.Data); got != len(payload) {
		t.Fatalf("translated payload = %d bytes, want %d", got, len(payload))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(payload))*6/5 {
		t.Errorf("translating a %d-byte image allocated %d bytes; want one copy of the payload, not several", len(payload), allocated)
	}
}

func TestStringFormImageURLIsRefusedWhenImagesAreOff(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/cat.png"}]}]}`)
	const want = "unsupported content part: image_url"
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || err.Error() != want {
		t.Errorf("anthropic err = %v, want %q", err, want)
	}
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || err.Error() != want {
		t.Errorf("responses err = %v, want %q", err, want)
	}
}

func TestMalformedImageURLIsRefusedWhenImagesAreOn(t *testing.T) {
	const want = "image_url must be an object with a url"
	for name, value := range map[string]string{
		"string": `"https://example.com/cat.png"`,
		"number": `7`,
		"array":  `[]`,
		"null":   `null`,
	} {
		t.Run(name, func(t *testing.T) {
			req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":`+value+`}]}]}`)
			if _, err := ToAnthropicWith(req, visionAnthropic); err == nil || err.Error() != want {
				t.Errorf("anthropic err = %v, want %q", err, want)
			}
			if _, err := ToResponsesWith(req, visionResponses); err == nil || err.Error() != want {
				t.Errorf("responses err = %v, want %q", err, want)
			}
		})
	}
}

func TestStandardBase64WithTrailingPaddingIsAccepted(t *testing.T) {
	for _, payload := range []string{"AAAA", "AAA=", "AA==", "a+/9", "Zm9v+/Zm9v=="} {
		req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,`+payload+`"}}]}]}`)
		if _, err := ToAnthropicWith(req, visionAnthropic); err != nil {
			t.Errorf("payload %q refused: %v", payload, err)
		}
	}
}

const errNeedsType = "invalid message content: a content part needs a type"

func errForbiddenKey(key string) string {
	return "invalid message content: a content part may not carry a \"" + key + "\" key; a passthrough row forwards only text, image_url and refusal parts"
}

func TestCountImagePartsSeesEveryKeyOccurrence(t *testing.T) {
	const image = `{"url":"https://example.com/a.png"}`
	cases := []struct {
		name    string
		content string
		want    int
		refused string
	}{
		{"plain image", `[{"type":"image_url","image_url":` + image + `}]`, 1, ""},
		{"text only", `[{"type":"text","text":"hi"}]`, 0, ""},
		{"string content", `"an image_url in prose"`, 0, ""},
		{"no content", ``, 0, ""},
		{"null content", `null`, 0, ""},
		{"shadowing Type key", `[{"type":"image_url","Type":"text","image_url":` + image + `}]`, 1, ""},
		{"shadowing TYPE after the real key", `[{"type":"image_url","TYPE":"text","image_url":` + image + `}]`, 1, ""},
		{"escaped shadowing key", `[{"type":"image_url","\u0054YPE":"text","image_url":` + image + `}]`, 1, ""},
		{"duplicate type, last is text", `[{"type":"image_url","type":"text","image_url":` + image + `}]`, 1, ""},
		{"escaped type value", `[{"type":"image\u005furl"}]`, 1, ""},
		{"only a differently cased type key", `[{"Type":"image_url"}]`, 1, ""},
		{"image_url key without type", `[{"image_url":` + image + `}]`, 0, "invalid message content: a content part needs a type"},
		{"differently cased image_url key", `[{"type":"text","Image_URL":` + image + `}]`, 1, ""},
		{"null image_url counts nothing", `[{"type":"text","text":"hi","image_url":null}]`, 0, ""},
		{"empty object image_url", `[{"type":"text","image_url":{}}]`, 1, ""},
		{"empty string image_url", `[{"type":"text","image_url":""}]`, 1, ""},
		{"string image_url", `[{"type":"text","image_url":"https://example.com/a.png"}]`, 1, ""},
		{"re-cased null image_url counts nothing", `[{"type":"text","Image_URL":null}]`, 0, ""},
		{"number beyond float64 range", `[{"type":"text","text":"x","n":1e400}]`, 0, ""},
		{"image beside a part with a huge number", `[{"type":"image_url"},{"type":"text","text":"x","n":1e400}]`, 1, ""},
		{"image_url nested in another key is not a part", `[{"type":"text","meta":{"image_url":1}}]`, 0, ""},
		{"two images", `[{"type":"image_url"},{"type":"text"},{"type":"image_url"}]`, 2, ""},
		{"refusal part", `[{"type":"refusal","refusal":"no"}]`, 0, ""},
		{"part without a type key", `[{"text":"hi"}]`, 0, errNeedsType},
		{"empty object part", `[{}]`, 0, errNeedsType},
		{"file part without a type", `[{"file":{"file_id":"f"}}]`, 0, errNeedsType},
		{"input_audio part without a type", `[{"input_audio":{"data":"AAAA","format":"wav"}}]`, 0, errNeedsType},
		{"file_id part without a type", `[{"file_id":"f"}]`, 0, errNeedsType},
		{"typeless part beside a typed one", `[{"type":"text","text":"hi"},{"text":"hi"}]`, 0, errNeedsType},
		{"empty array", `[]`, 0, ""},
		{"text with cache_control", `[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]`, 0, ""},
		{"inline image", `[{"type":"image_url","image_url":{"url":"` + pngDataURI + `","detail":"low"}}]`, 1, ""},
		{"text carrying a file", `[{"type":"text","file":{"file_id":"f"}}]`, 0, errForbiddenKey("file")},
		{"text carrying a re-cased FILE", `[{"type":"text","FILE":{"file_id":"f"}}]`, 0, errForbiddenKey("FILE")},
		{"text carrying a file_id", `[{"type":"text","file_id":"f"}]`, 0, errForbiddenKey("file_id")},
		{"text carrying input_audio", `[{"type":"text","input_audio":{"data":"AAAA","format":"wav"}}]`, 0, errForbiddenKey("input_audio")},
		{"image carrying a file", `[{"type":"image_url","image_url":` + image + `,"file":"f"}]`, 0, errForbiddenKey("file")},
		{"file key spelled with an escape", `[{"type":"text","\u0066ile":1}]`, 0, errForbiddenKey("file")},
		{"second file key is the non-null one", `[{"type":"text","file":null,"File":1}]`, 0, errForbiddenKey("File")},
		{"text with a null file", `[{"type":"text","file":null}]`, 0, ""},
		{"file nested in another key is not a part", `[{"type":"text","meta":{"file":1}}]`, 0, ""},
		{"file part", `[{"type":"file","file":{"file_id":"f"}}]`, 0, "invalid message content: unsupported content part type \"file\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"input_audio part", `[{"type":"text"},{"type":"input_audio"}]`, 0, "invalid message content: unsupported content part type \"input_audio\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"invented type", `[{"type":"hologram"}]`, 0, "invalid message content: unsupported content part type \"hologram\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"unsupported type shadowed by a later text type", `[{"type":"file","TYPE":"text"}]`, 0, "invalid message content: unsupported content part type \"file\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"unsupported type beside an image", `[{"type":"image_url","Type":"x"}]`, 0, "invalid message content: unsupported content part type \"x\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"long unsupported type is truncated", `[{"type":"` + strings.Repeat("y", 1<<16) + `"}]`, 0, "invalid message content: unsupported content part type \"" + strings.Repeat("y", echoLimit) + "…\"; a passthrough row forwards only text, image_url and refusal parts"},
		{"non-string type", `[{"type":"image_url","Type":5}]`, 0, "invalid message content: a content part's type must be a string"},
		{"sibling with a non-string type", `[{"type":"image_url"},{"type":5}]`, 0, "invalid message content: a content part's type must be a string"},
		{"bare string element", `[{"type":"image_url"},"x"]`, 0, "invalid message content: content parts must be objects"},
		{"nested array element", `[[{"type":"image_url"}]]`, 0, "invalid message content: content parts must be objects"},
		{"object content", `{"type":"image_url"}`, 0, "invalid message content: content must be a string or an array of content parts"},
		{"number content", `5`, 0, "invalid message content: content must be a string or an array of content parts"},
		{"truncated array", `[{"type":"image_url"}`, 0, "invalid message content: content is not valid JSON"},
		{"trailing data", `[{"type":"image_url"}] 1`, 0, "invalid message content: content is not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CountImageParts([]oai.Message{{Role: "user", Content: json.RawMessage(tc.content)}})
			switch {
			case tc.refused != "" && (err == nil || err.Error() != tc.refused):
				t.Errorf("err = %v, want %q", err, tc.refused)
			case tc.refused == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tc.refused == "" && got != tc.want:
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCountImagePartsSumsAcrossMessages(t *testing.T) {
	req := chatRequest(t, visionTurn)
	got, err := CountImageParts(append(req.Messages, req.Messages...))
	if err != nil || got != 4 {
		t.Errorf("count = %d, %v; want 4", got, err)
	}
}
