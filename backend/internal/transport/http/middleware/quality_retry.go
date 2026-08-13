package middleware

import (
	"crypto/subtle"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Quality retry headers are accepted only when the process-scoped token is
// valid. They are intended for the local quality retry proxy and are never a
// public account-selection API.
const (
	QualityRetryTokenHeader           = "X-Grok-Quality-Retry-Token"
	QualityRetryExcludeAccountsHeader = "X-Grok-Quality-Retry-Exclude-Accounts"
	QualityRetryAccountHeader         = "X-Grok-Quality-Retry-Account-ID"
	QualityRetryRequestKey            = "qualityRetryRequest"
	QualityRetryExcludedAccountsKey   = "qualityRetryExcludedAccounts"
	qualityRetryMaxExcludedAccounts   = 64
)

// QualityRetry parses the process-scoped quality retry metadata. Invalid or
// absent metadata is ignored so ordinary API requests retain their behavior.
func QualityRetry(expectedToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken := strings.TrimSpace(c.GetHeader(QualityRetryTokenHeader))
		if isLocalQualityRetryPeer(c.Request.RemoteAddr) &&
			expectedToken != "" && len(rawToken) == len(expectedToken) &&
			subtle.ConstantTimeCompare([]byte(rawToken), []byte(expectedToken)) == 1 {
			c.Set(QualityRetryRequestKey, true)
			c.Set(QualityRetryExcludedAccountsKey, parseQualityRetryAccountIDs(c.GetHeader(QualityRetryExcludeAccountsHeader)))
		}
		c.Next()
	}
}

func isLocalQualityRetryPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return address.IsLoopback() || address.IsPrivate()
}

// QualityRetryMetadata returns validated request metadata for inference
// handlers. The returned slice is always detached from Gin's context value.
func QualityRetryMetadata(c *gin.Context) (bool, []uint64) {
	value, ok := c.Get(QualityRetryRequestKey)
	if !ok {
		return false, nil
	}
	enabled, ok := value.(bool)
	if !ok || !enabled {
		return false, nil
	}
	value, _ = c.Get(QualityRetryExcludedAccountsKey)
	ids, _ := value.([]uint64)
	return true, append([]uint64(nil), ids...)
}

func parseQualityRetryAccountIDs(value string) []uint64 {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	result := make([]uint64, 0, min(qualityRetryMaxExcludedAccounts, 8))
	seen := make(map[uint64]struct{}, cap(result))
	for _, part := range strings.Split(value, ",") {
		if len(result) >= qualityRetryMaxExcludedAccounts {
			break
		}
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || id == 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
