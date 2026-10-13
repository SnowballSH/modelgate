package translate

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
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

func TestMediaTypeIsMatchedWithoutRegardToCase(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:IMAGE/JPEG;base64,`+pngBase64+`"}}]}]}`)
	areq, err := ToAnthropicWith(req, visionAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if got := areq.Messages[0].Content[0].Source.MediaType; got != "image/jpeg" {
		t.Errorf("media_type = %q, want image/jpeg", got)
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
		`{"messages":[{"role":"user","content":"a string mentioning image_url"}]}`:                        false,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`:                          false,
		`{"messages":[{"role":"system","content":[{"type":"image_url"}]},{"role":"user","content":"x"}]}`: true,
		`{"messages":[{"role":"assistant"}]}`:                                                             false,
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
