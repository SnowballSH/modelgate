package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

const (
	imagePartType      = "image_url"
	anthropicImageType = "image"
	responsesImageType = "input_image"
	dataURIScheme      = "data:"
	echoLimit          = 64
)

var (
	imageMediaTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
	imageDetails    = []string{"auto", "low", "high"}
	pricedPartTypes = []string{"text", imagePartType, "refusal"}
)

// echoed bounds a client string quoted back in an error message.
func echoed(value string) string {
	if len(value) <= echoLimit {
		return value
	}
	cut := echoLimit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

type image struct {
	url       string
	mediaType string
	data      string
	detail    string
}

type imageRef struct {
	URL    string `json:"url"`
	Detail string `json:"detail"`
}

// borrowedJSON keeps the bytes it was decoded from instead of copying them,
// which json.RawMessage would: an inline image is megabytes. The caller's
// content buffer outlives the decode and is never written to.
type borrowedJSON []byte

func (b *borrowedJSON) UnmarshalJSON(data []byte) error {
	*b = data[:len(data):len(data)]
	return nil
}

var errImageRefShape = errors.New("image_url must be an object with a url")

func decodeImageRef(raw borrowedJSON) (imageRef, error) {
	var ref *imageRef
	if json.Unmarshal(raw, &ref) != nil || ref == nil {
		return imageRef{}, errImageRefShape
	}
	return *ref, nil
}

var errImageAlphabet = errors.New("image_url.url data URI payload must use only the standard base64 alphabet (A-Z, a-z, 0-9, + and /, with = only at the end)")

// isStandardBase64 holds every accepted payload to the standard alphabet and
// whole four-character groups, so it decodes to exactly the bytes the body
// carried.
func isStandardBase64(data string) bool {
	body := strings.TrimRight(data, "=")
	if len(data)-len(body) > 2 || body == "" || len(data)%4 != 0 {
		return false
	}
	for i := range len(body) {
		c := body[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '/' {
			return false
		}
	}
	return true
}

func parseImage(rawURL, detail string) (image, error) {
	if detail != "" && !slices.Contains(imageDetails, detail) {
		return image{}, fmt.Errorf("image_url.detail %q is not one of %s", echoed(detail), strings.Join(imageDetails, ", "))
	}
	if strings.HasPrefix(rawURL, "https://") {
		return image{url: rawURL, detail: detail}, nil
	}
	rest, isData := strings.CutPrefix(rawURL, dataURIScheme)
	if !isData {
		return image{}, errors.New("image_url.url must be a data: URI or an https URL")
	}
	header, data, _ := strings.Cut(rest, ",")
	mediaType, isBase64 := strings.CutSuffix(header, ";base64")
	if !isBase64 || data == "" {
		return image{}, errors.New("image_url.url data URI must carry base64 data")
	}
	if !isStandardBase64(data) {
		return image{}, errImageAlphabet
	}
	if !slices.Contains(imageMediaTypes, mediaType) {
		return image{}, fmt.Errorf("unsupported image media type %q; use one of %s", echoed(mediaType), strings.Join(imageMediaTypes, ", "))
	}
	return image{url: rawURL, mediaType: mediaType, data: data, detail: detail}, nil
}

func (i image) anthropicBlock() anthro.ContentBlock {
	if i.data == "" {
		return anthro.ContentBlock{Type: anthropicImageType, Source: &anthro.ImageSource{Type: "url", URL: i.url}}
	}
	return anthro.ContentBlock{Type: anthropicImageType, Source: &anthro.ImageSource{Type: "base64", MediaType: i.mediaType, Data: i.data}}
}

func (i image) responsesPart() oairesp.ContentPart {
	return oairesp.ContentPart{Type: responsesImageType, ImageURL: i.url, Detail: i.detail}
}

// ImageLoad is what a translated request's images weigh: how many there are,
// and how many body bytes are inline image data rather than prompt text.
type ImageLoad struct {
	Count       int
	InlineBytes int
}

func AnthropicImageLoad(req anthro.MessagesRequest) ImageLoad {
	var load ImageLoad
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Type == anthropicImageType && block.Source != nil {
				load.Count++
				load.InlineBytes += len(block.Source.Data)
			}
		}
	}
	return load
}

func ResponsesImageLoad(req oairesp.Request) ImageLoad {
	var load ImageLoad
	for _, item := range req.Input {
		for _, part := range item.Content {
			if part.Type != responsesImageType {
				continue
			}
			load.Count++
			if strings.HasPrefix(part.ImageURL, dataURIScheme) {
				load.InlineBytes += len(part.ImageURL)
			}
		}
	}
	return load
}

var (
	errContentShape      = errors.New("invalid message content: content must be a string or an array of content parts")
	errContentNotObjects = errors.New("invalid message content: content parts must be objects")
	errContentPartType   = errors.New("invalid message content: a content part's type must be a string")
	errContentJSON       = errors.New("invalid message content: content is not valid JSON")
)

func unsupportedPartType(partType string) error {
	return fmt.Errorf("invalid message content: unsupported content part type %q; a passthrough row forwards only text, image_url and refusal parts", echoed(partType))
}

func isPricedPartType(partType string) bool {
	return slices.ContainsFunc(pricedPartTypes, func(priced string) bool { return strings.EqualFold(partType, priced) })
}

// CountImageParts counts the parts a provider could read as images from
// content forwarded verbatim. It reads tokens rather than decoding into a
// struct because a struct decode lets a duplicate or re-cased key hide a part.
func CountImageParts(messages []oai.Message) (int, error) {
	total := 0
	for _, msg := range messages {
		n, err := countContentImages(msg.Content)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func countContentImages(content json.RawMessage) (int, error) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || string(content) == "null" || content[0] == '"' {
		return 0, nil
	}
	if content[0] != '[' {
		return 0, errContentShape
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil {
		return 0, errContentJSON
	}
	count := 0
	for dec.More() {
		open, err := dec.Token()
		if err != nil {
			return 0, errContentJSON
		}
		if delim, ok := open.(json.Delim); !ok || delim != '{' {
			return 0, errContentNotObjects
		}
		isImage, err := scanPart(dec)
		if err != nil {
			return 0, err
		}
		if isImage {
			count++
		}
	}
	if _, err := dec.Token(); err != nil {
		return 0, errContentJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, errContentJSON
	}
	return count, nil
}

func scanPart(dec *json.Decoder) (bool, error) {
	isImage := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return false, errContentJSON
		}
		key, _ := keyToken.(string)
		switch {
		case strings.EqualFold(key, "type"):
			valueToken, err := dec.Token()
			if err != nil {
				return false, errContentJSON
			}
			value, ok := valueToken.(string)
			if !ok {
				return false, errContentPartType
			}
			if !isPricedPartType(value) {
				return false, unsupportedPartType(value)
			}
			isImage = isImage || strings.EqualFold(value, imagePartType)
		case strings.EqualFold(key, imagePartType):
			first, err := skipValue(dec)
			if err != nil {
				return false, err
			}
			isImage = isImage || first != nil
		default:
			if _, err := skipValue(dec); err != nil {
				return false, err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return false, errContentJSON
	}
	return isImage, nil
}

// skipValue consumes one value and returns its first token, nil for JSON null.
func skipValue(dec *json.Decoder) (json.Token, error) {
	first, err := dec.Token()
	if err != nil {
		return nil, errContentJSON
	}
	if delim, ok := first.(json.Delim); !ok || delim == '}' || delim == ']' {
		return first, nil
	}
	for depth := 1; depth > 0; {
		token, err := dec.Token()
		if err != nil {
			return nil, errContentJSON
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return first, nil
}

type typedPart struct {
	Type string `json:"type"`
}

// HasImageParts decides as the translators' own decode does: a part is an
// image when its type, as Go's decoder resolves it, is image_url.
func HasImageParts(messages []oai.Message) bool {
	return slices.ContainsFunc(messages, func(msg oai.Message) bool {
		var parts []typedPart
		if json.Unmarshal(msg.Content, &parts) != nil {
			return false
		}
		return slices.ContainsFunc(parts, func(part typedPart) bool {
			return part.Type == imagePartType
		})
	})
}
