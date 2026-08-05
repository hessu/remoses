package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// specPath is the contract itself. The handlers are hand-written rather than
// generated, so something has to notice when one side moves: this test is that
// something.
const specPath = "../../api/openapi.yaml"

// httpMethods are the keys of an OpenAPI path item that describe operations.
// Everything else in a path item — "parameters", "summary" — is not one.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

type specOperation struct {
	method      string // upper case
	path        string
	operationID string
}

func loadSpec(t *testing.T) []specOperation {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s declares no paths", specPath)
	}

	var ops []specOperation
	for path, item := range doc.Paths {
		for key, body := range item {
			if !slices.Contains(httpMethods, strings.ToLower(key)) {
				continue
			}
			op := specOperation{method: strings.ToUpper(key), path: path}
			if m, ok := body.(map[string]any); ok {
				op.operationID, _ = m["operationId"].(string)
			}
			ops = append(ops, op)
		}
	}
	slices.SortFunc(ops, func(a, b specOperation) int {
		return strings.Compare(a.path+" "+a.method, b.path+" "+b.method)
	})
	return ops
}

func TestEverySpecOperationHasARoute(t *testing.T) {
	e := newEnv(t)

	registered := map[string]bool{}
	for _, rt := range e.srv.routes() {
		registered[rt.method+" "+rt.path] = true
	}

	for _, op := range loadSpec(t) {
		key := op.method + " " + op.path
		if !registered[key] {
			t.Errorf("openapi.yaml declares %s (%s) with no route registered", key, op.operationID)
		}
	}
}

func TestEveryRouteIsInTheSpec(t *testing.T) {
	e := newEnv(t)

	declared := map[string]bool{}
	for _, op := range loadSpec(t) {
		declared[op.method+" "+op.path] = true
	}

	for _, rt := range e.srv.routes() {
		key := rt.method + " " + rt.path
		if !declared[key] {
			t.Errorf("route %s is registered but not declared in openapi.yaml", key)
		}
	}
}

// Registering a pattern is not the same as being reachable through it: a base
// path or placeholder spelt wrongly would still appear in the route table.
func TestEverySpecOperationIsReachable(t *testing.T) {
	e := newEnv(t)

	for _, op := range loadSpec(t) {
		t.Run(op.operationID, func(t *testing.T) {
			rr := e.do(op.method, pathFor(op.path, connectedRadio), nil)
			if rr.Code == http.StatusNotFound &&
				strings.Contains(rr.Body.String(), "no such endpoint") {
				t.Errorf("%s %s reached the catch-all: the pattern does not match",
					op.method, op.path)
			}
		})
	}
}
