package translate

import (
	"crypto/sha256"
	"encoding/hex"
)

const cacheKeyDigestBytes = 16

// NamespacedCacheKey turns a client's prompt_cache_key into the value sent
// upstream: a digest over the gateway key's id and the client's value, so two
// gateway keys never share a routing bucket and the result has one fixed
// length whatever the client sent.
func NamespacedCacheKey(gatewayKeyID, clientKey string) string {
	if clientKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(gatewayKeyID + "\x00" + clientKey))
	return hex.EncodeToString(sum[:cacheKeyDigestBytes])
}
