package middleware

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestQualityRetryAcceptsOnlyScopedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		token    string
		remote   string
		enabled  bool
		excluded []uint64
	}{
		{name: "valid loopback", token: "scoped-token", remote: "127.0.0.1:18001", enabled: true, excluded: []uint64{31, 32}},
		{name: "valid docker private", token: "scoped-token", remote: "172.17.0.1:18001", enabled: true, excluded: []uint64{31, 32}},
		{name: "public peer rejected", token: "scoped-token", remote: "203.0.113.8:18001", enabled: false},
		{name: "invalid", token: "wrong-token", remote: "127.0.0.1:18001", enabled: false},
		{name: "missing", remote: "127.0.0.1:18001", enabled: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			context.Request.RemoteAddr = test.remote
			if test.token != "" {
				context.Request.Header.Set(QualityRetryTokenHeader, test.token)
			}
			context.Request.Header.Set(QualityRetryExcludeAccountsHeader, "31, invalid,32,31,0")
			QualityRetry("scoped-token")(context)
			enabled, excluded := QualityRetryMetadata(context)
			if enabled != test.enabled || !reflect.DeepEqual(excluded, test.excluded) {
				t.Fatalf("metadata = %v, %#v", enabled, excluded)
			}
		})
	}
}

func TestParseQualityRetryAccountIDsIsBoundedAndDeduplicated(t *testing.T) {
	value := ""
	for id := 1; id <= qualityRetryMaxExcludedAccounts+10; id++ {
		if value != "" {
			value += ","
		}
		value += strconv.Itoa(id)
	}
	ids := parseQualityRetryAccountIDs("31,32,31,invalid,0")
	if !reflect.DeepEqual(ids, []uint64{31, 32}) {
		t.Fatalf("ids = %#v", ids)
	}
	if len(parseQualityRetryAccountIDs(value)) > qualityRetryMaxExcludedAccounts {
		t.Fatal("excluded account list exceeded its bound")
	}
}
