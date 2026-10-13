package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

const (
	imagePartType      = "image_url"
	anthropicImageType = "image"
	responsesImageType = "input_image"
	dataURIScheme      = "data:"
)

var (
	imageMediaTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
	imageDetails    = []string{"auto", "low", "high"}
)

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
	*b = data
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

// isStandardBase64 holds every accepted payload to one byte per byte, so the
// decoded string's length is the length it occupies in the request body.
func isStandardBase64(data string) bool {
	body := strings.TrimRight(data, "=")
	if len(data)-len(body) > 2 {
		return false
	}
	for i := range len(body) {
		c := body[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}

func parseImage(rawURL, detail string) (image, error) {
	if detail != "" && !slices.Contains(imageDetails, detail) {
		return image{}, fmt.Errorf("image_url.detail %q is not one of %s", detail, strings.Join(imageDetails, ", "))
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
	mediaType = strings.ToLower(mediaType)
	if !slices.Contains(imageMediaTypes, mediaType) {
		return image{}, fmt.Errorf("unsupported image media type %q; use one of %s", mediaType, strings.Join(imageMediaTypes, ", "))
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

// CountImageParts reads only each part's type, so counting costs no copy of
// an inline image.
func CountImageParts(messages []oai.Message) int {
	count := 0
	for _, msg := range messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == imagePartType {
				count++
			}
		}
	}
	return count
}

func HasImageParts(messages []oai.Message) bool {
	return CountImageParts(messages) > 0
}
