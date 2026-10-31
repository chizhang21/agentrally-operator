package connector

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestConfigMapCreationRejectsUnsafeBodies(t *testing.T) {
	for _, body := range []string{
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team","annotations":{"private":"sensitive-marker"}},"data":{"key":"value"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team","ownerReferences":[]},"data":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"other"},"data":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team"},"stringData":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team"},"binaryData":{"key":"not valid Base64!"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team"}}{}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"agentrally-team"}}` + strings.Repeat(" ", MaxMessageBytes),
	} {
		kube, req := managementFixture(t)
		req.Resource, req.Method, req.Name, req.Body = "configmaps", http.MethodPost, "", json.RawMessage(body)
		response := (Executor{Client: kube}).Execute(t.Context(), req)
		if response.Status != http.StatusBadRequest || strings.Contains(string(response.Body), "sensitive-marker") {
			t.Fatalf("unsafe ConfigMap body was accepted or reflected: %s", response.Body)
		}
		for _, action := range kube.Actions() {
			if action.GetVerb() != "get" {
				t.Fatal("rejected ConfigMap request produced a write")
			}
		}
	}
}
