package admin

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func TestEndpointTLSCoversTheConfiguredNamesAndReissuesOnChange(t *testing.T) {
	var stored Object
	writes := 0
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch method {
		case "GET":
			if stored == nil {
				return nil, nil
			}
			return stored, nil
		case "POST", "PUT":
			writes++
			stored = jsonx.Clone(b.(Object))
			jsonx.Obj(stored["metadata"])["resourceVersion"] = "1"
		}
		return Object{}, nil
	})}
	c := Config{RunnerEndpoint: RunnerEndpoint{Names: []string{"panko.example.com", "192.0.2.10"}}}
	for i := 0; i < 2; i++ {
		if err := m.EnsureEndpointTLS(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatal("unchanged certificate reissued", writes)
	}
	data, _ := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(stored["data"])["tls.crt"]))
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"panko.example.com", "192.0.2.10", "runner-endpoint.infra-runners.svc"} {
		if cert.VerifyHostname(name) != nil {
			t.Fatalf("certificate does not cover %s", name)
		}
	}
	c.RunnerEndpoint.Names = append(c.RunnerEndpoint.Names, "edge.example.com")
	if err := m.EnsureEndpointTLS(context.Background(), c); err != nil || writes != 2 {
		t.Fatal("new endpoint name did not reissue the certificate", err, writes)
	}
	c.RunnerEndpoint.Names = c.RunnerEndpoint.Names[1:]
	if err := m.EnsureEndpointTLS(context.Background(), c); err != nil || writes != 3 {
		t.Fatal("removed endpoint name did not reissue the certificate", err, writes)
	}
	if _, err := m.EndpointCA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (RunnerEndpoint{Names: []string{"bad name"}}).validate() == nil {
		t.Fatal("invalid endpoint name accepted")
	}
}
