package admin

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestEndpointTLSCoversTheConfiguredNamesAndReissuesOnChange(t *testing.T) {
	var stored, ca Object
	writes := 0
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		isCA := strings.HasSuffix(path, "-ca") || (b != nil && jsonx.Str(jsonx.Obj(jsonx.Obj(b)["metadata"])["name"]) == "runner-endpoint-tls-ca")
		switch method {
		case "GET":
			if isCA {
				if ca == nil {
					return nil, &kube.Error{Code: 404}
				}
				return ca, nil
			}
			if stored == nil {
				return nil, &kube.Error{Code: 404}
			}
			return stored, nil
		case "POST", "PUT":
			if isCA {
				ca = jsonx.Clone(b.(Object))
				return Object{}, nil
			}
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
	first, err := m.EndpointCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Renewal keeps the CA: agents need no new trust when the server certificate changes.
	c.RunnerEndpoint.Names = append(c.RunnerEndpoint.Names, "renewed.example.com")
	if err := m.EnsureEndpointTLS(context.Background(), c); err != nil || writes != 4 {
		t.Fatal("certificate not reissued", err, writes)
	}
	if again, _ := m.EndpointCA(context.Background()); string(again) != string(first) {
		t.Fatal("reissuing the certificate replaced the CA")
	}
	if jsonx.Obj(stored["data"])["ca.key"] != nil {
		t.Fatal("the serving Secret holds the CA key")
	}
	if (RunnerEndpoint{Names: []string{"bad name"}}).validate() == nil {
		t.Fatal("invalid endpoint name accepted")
	}
}
