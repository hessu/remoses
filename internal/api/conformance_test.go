package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/hessu/remoses/internal/wire"
)

// specPath is the contract itself. The handlers are hand-written rather than
// generated, so something has to notice when one side moves: this test is that
// something.
//
// It notices in three ways. The route tests below compare the operations the
// document declares against the ones the server registers. The decode tests
// push real responses through internal/wire — the Go the document generates —
// with unknown fields refused, so a field the daemon sends and the spec does
// not declare fails here rather than in somebody's client. And the required
// test reads the promises the document makes and checks the daemon keeps them.
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

// strictDecode decodes a response body into a generated type, refusing any
// member the type does not have.
//
// Refusing is the whole point. Decoding leniently — which is what a client does
// — would accept a body carrying three fields nobody ever wrote down, and the
// first anyone would hear of them is when a generated client silently dropped
// them.
func strictDecode(t *testing.T, rr *httptest.ResponseRecorder, want int, dst any) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", rr.Code, want, rr.Body.String())
	}
	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("decoding %s into %T: %v", rr.Body.String(), dst, err)
	}
}

// TestResponsesDecodeIntoTheGeneratedTypes runs the daemon's own answers
// through the types api/openapi.yaml generates, which is what remoses-cli and
// any other generated client will read them with.
func TestResponsesDecodeIntoTheGeneratedTypes(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	t.Run("Radio", func(t *testing.T) {
		var got wire.Radio
		strictDecode(t, e.do(http.MethodGet, "/radios/"+connectedRadio, nil), http.StatusOK, &got)
		if got.ID != connectedRadio || got.Caps.SMeterScale == 0 {
			t.Errorf("descriptor = %+v", got)
		}
	})

	t.Run("RadioList", func(t *testing.T) {
		var got []wire.Radio
		strictDecode(t, e.do(http.MethodGet, "/radios", nil), http.StatusOK, &got)
		if len(got) != 2 {
			t.Errorf("radios = %d, want 2", len(got))
		}
	})

	// Both radios, because the two exercise different halves of the schema: a
	// started one reports readings, and one that never connected reports the
	// absences.
	for _, id := range []string{connectedRadio, disconnectedRadio} {
		t.Run("State/"+id, func(t *testing.T) {
			var got wire.State
			strictDecode(t, e.do(http.MethodGet, "/radios/"+id+"/state", nil), http.StatusOK, &got)
			if got.UpdatedAt.IsZero() {
				t.Error("updated_at did not decode")
			}
		})
	}

	// The response to a write, which is read back from the rig and so can carry
	// fields a cache read does not.
	t.Run("StateAfterPatch", func(t *testing.T) {
		var got wire.State
		body := wire.StatePatch{Frequency: ptr(int64(14030000)), Mode: ptr(wire.ModeCW)}
		rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state", body, token)
		strictDecode(t, rr, http.StatusOK, &got)
		if got.Frequency != 14030000 {
			t.Errorf("frequency = %d, want the patched 14030000", got.Frequency)
		}
	})

	t.Run("CWStatus", func(t *testing.T) {
		var got wire.CWStatus
		strictDecode(t, e.do(http.MethodGet, "/radios/"+connectedRadio+"/cw", nil),
			http.StatusOK, &got)
	})

	t.Run("CWAccepted", func(t *testing.T) {
		var got wire.CWAccepted
		rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
			wire.CWRequest{Text: "TEST DE N0CALL"}, token)
		strictDecode(t, rr, http.StatusAccepted, &got)
		if got.QueuedChars == 0 {
			t.Error("queued_chars = 0 for text that was accepted")
		}
	})

	t.Run("LockState", func(t *testing.T) {
		var got wire.LockState
		strictDecode(t, e.do(http.MethodGet, "/radios/"+connectedRadio+"/lock", nil),
			http.StatusOK, &got)
		if !got.Held {
			t.Error("held = false while this test holds the lock")
		}
	})

	t.Run("Lock", func(t *testing.T) {
		var got wire.Lock
		strictDecode(t, e.do(http.MethodPost, "/radios/"+disconnectedRadio+"/lock", nil),
			http.StatusCreated, &got)
		if got.Token == "" || got.TTLSeconds == 0 {
			t.Errorf("lock = %+v", got)
		}
	})

	// The error shape matters as much as the success one: a client that cannot
	// decode a problem document reports "something went wrong" instead of the
	// sentence the daemon wrote.
	t.Run("Problem", func(t *testing.T) {
		var got wire.Problem
		strictDecode(t, e.do(http.MethodGet, "/radios/nosuchradio/state", nil),
			http.StatusNotFound, &got)
		if got.Title == "" || got.RadioID == nil {
			t.Errorf("problem = %+v, want a title and the radio_id extension", got)
		}
	})
}

// TestResponsesCarryEveryRequiredField reads the promises out of the document
// and checks the daemon keeps them.
//
// It is the other direction from the decode tests: those catch a field the
// daemon sends and the spec does not declare, and this catches a field the spec
// says is always there and the daemon leaves out. A generated client turns a
// required field into a plain value rather than a pointer, so a missing one is
// not an error it can notice — it is a zero that reads as a real reading.
func TestResponsesCarryEveryRequiredField(t *testing.T) {
	e := newEnv(t)
	schemas := loadSchemas(t)

	cases := []struct {
		schema string
		rr     *httptest.ResponseRecorder
	}{
		{"Radio", e.do(http.MethodGet, "/radios/"+connectedRadio, nil)},
		{"State", e.do(http.MethodGet, "/radios/"+connectedRadio+"/state", nil)},
		{"State", e.do(http.MethodGet, "/radios/"+disconnectedRadio+"/state", nil)},
		{"CWStatus", e.do(http.MethodGet, "/radios/"+connectedRadio+"/cw", nil)},
		{"LockState", e.do(http.MethodGet, "/radios/"+connectedRadio+"/lock", nil)},
		{"Lock", e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", nil)},
	}
	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(tc.rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding %s: %v", tc.rr.Body.String(), err)
			}
			for _, name := range requiredFields(t, schemas, tc.schema) {
				if _, ok := body[name]; !ok {
					t.Errorf("%s promises %q and the response does not carry it: %s",
						tc.schema, name, tc.rr.Body.String())
				}
			}
		})
	}
}

// loadSchemas returns components/schemas as it stands in the document.
func loadSchemas(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatalf("%s declares no schemas", specPath)
	}
	return doc.Components.Schemas
}

// requiredFields names the members a schema promises, following $ref and allOf.
// State is spelled as StateFields plus a required list, so a resolver that
// stopped at the first node would find nothing at all.
func requiredFields(t *testing.T, schemas map[string]any, name string) []string {
	t.Helper()
	out := map[string]bool{}
	collectRequired(t, schemas, schemas[name], out, 0)
	return slices.Sorted(maps.Keys(out))
}

func collectRequired(t *testing.T, schemas map[string]any, node any, out map[string]bool, depth int) {
	t.Helper()
	if depth > 8 {
		t.Fatalf("schema nesting is deeper than expected; a $ref cycle?")
	}
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, "#/components/schemas/")
		if !found {
			t.Fatalf("unsupported $ref %q", ref)
		}
		collectRequired(t, schemas, schemas[name], out, depth+1)
	}
	for _, sub := range asSlice(m["allOf"]) {
		collectRequired(t, schemas, sub, out, depth+1)
	}
	for _, r := range asSlice(m["required"]) {
		if s, ok := r.(string); ok {
			out[s] = true
		}
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func ptr[T any](v T) *T { return &v }

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
