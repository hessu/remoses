package ws

import (
	"reflect"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// changedFields is a hand-maintained mapping from radio.Patch onto State's JSON
// names, and hand-maintained parallel lists drift. This one did: standby,
// tuner, power_meter and swr_ratio were all added to the state and to Diff, and
// none of them reached a WebSocket client. The symptom was a display that was
// right when it started and never updated afterwards — a fresh client fetched
// the snapshot over REST and saw the field, and no delta ever moved it again.
//
// So rather than listing the fields here too, this walks radio.Patch and
// asserts that setting any one of them produces a delta. A new field fails this
// test until changedFields knows about it.
func TestEveryPatchFieldProducesADelta(t *testing.T) {
	pt := reflect.TypeOf(radio.Patch{})
	for i := range pt.NumField() {
		f := pt.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			var p radio.Patch
			pv := reflect.ValueOf(&p).Elem().Field(i)
			if pv.Kind() != reflect.Ptr {
				t.Skipf("%s is not a pointer field", f.Name)
			}
			// A non-nil field of the right type: "this changed".
			pv.Set(reflect.New(f.Type.Elem()))

			if got := changedFields(p, radio.State{}); len(got) == 0 {
				t.Errorf("a patch setting only %s produced no delta; "+
					"changedFields has not been taught about it, so a client "+
					"would never see that field change", f.Name)
			}
		})
	}
}

// The transmit meters vanish at the end of a transmission, and a client has to
// be told. A patch cannot say "this went away" — the new value is nil, which
// reads as "not mentioned" — so PTT dropping carries them out as explicit
// nulls.
func TestTransmitMetersAreClearedOnKeyDown(t *testing.T) {
	off := false
	got := changedFields(radio.Patch{PTT: &off}, radio.State{})

	for _, k := range []string{"power_meter", "swr", "alc", "swr_ratio"} {
		v, ok := got[k]
		if !ok {
			t.Errorf("%s missing from the delta; the last transmission's reading "+
				"would stay on a client's display", k)
			continue
		}
		if v != nil && !reflect.ValueOf(v).IsNil() {
			t.Errorf("%s = %v, want an explicit null", k, v)
		}
	}
}

// And while transmitting they carry values.
func TestTransmitMetersGoOutWithTheirValues(t *testing.T) {
	m := radio.Meter{Raw: 143, Scale: 255}
	st := radio.State{PTT: true, PowerMeter: &m}
	got := changedFields(radio.Patch{PowerMeter: &m}, st)

	pm, ok := got["power_meter"].(*radio.Meter)
	if !ok || pm == nil || pm.Raw != 143 {
		t.Errorf("power_meter = %#v, want the reading", got["power_meter"])
	}
}

// Standby is the one that prompted all this: a radio switched off at the front
// panel while a client was watching.
func TestStandbyProducesADelta(t *testing.T) {
	on := true
	got := changedFields(radio.Patch{Standby: &on}, radio.State{})
	if v, ok := got["standby"]; !ok || v != true {
		t.Errorf("standby delta = %v (present %v), want true", v, ok)
	}
}
