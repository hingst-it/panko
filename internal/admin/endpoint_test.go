package admin

import (
	"bytes"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func TestEndpointRBACGolden(t *testing.T) {
	var expected Object
	load(t, "endpoint-rbac.json", &expected)
	if !bytes.Equal(jsonx.Encode(EndpointRBAC()), jsonx.Encode(expected)) {
		t.Fatal("endpoint permissions changed")
	}
}
