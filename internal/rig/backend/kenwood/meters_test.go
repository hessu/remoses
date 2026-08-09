package kenwood

import (
	"context"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// SM is two meters in one command: the S-meter in receive and the RF power
// meter in transmit. It used to land in SMeter either way, which drove the
// receive signal bar to full scale for the length of every transmission.
func TestSMIsThePowerMeterWhileTransmitting(t *testing.T) {
	t.Run("receiving", func(t *testing.T) {
		k := newRig(t, 2, true)
		u, err := k.Decode([]byte("SM00015"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.SMeter == nil || u.Patch.SMeter.Raw != 15 {
			t.Errorf("S-meter = %+v, want raw 15", u.Patch.SMeter)
		}
		if u.Patch.PowerMeter != nil {
			t.Errorf("power meter = %+v, want nothing in receive", u.Patch.PowerMeter)
		}
	})

	t.Run("transmitting", func(t *testing.T) {
		k := newRig(t, 2, true)
		k.transmitting.Store(true)
		u, err := k.Decode([]byte("SM00015"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.PowerMeter == nil || u.Patch.PowerMeter.Raw != 15 {
			t.Errorf("power meter = %+v, want raw 15", u.Patch.PowerMeter)
		}
		if u.Patch.SMeter != nil {
			t.Errorf("S-meter = %+v, want it left alone while transmitting", u.Patch.SMeter)
		}
	})
}

// One RM; read draws three answers, and each is decoded on its own: the
// reference says "there are always three types of responses: SWR, COMP, and
// ALC".
func TestDecodeRM(t *testing.T) {
	k := newRig(t, 2, true)

	t.Run("SWR", func(t *testing.T) {
		u, err := k.Decode([]byte("RM10012"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Key != keyRM {
			t.Errorf("key = %q, want %q", u.Key, keyRM)
		}
		if u.Patch.SWR == nil || u.Patch.SWR.Raw != 12 || u.Patch.SWR.Scale != rmScale {
			t.Errorf("SWR = %+v, want raw 12 of %d", u.Patch.SWR, rmScale)
		}
		// No calibration is published for this meter, so no ratio is invented
		// from it.
		if u.Patch.SWRRatio != nil {
			t.Errorf("SWR ratio = %v, want none: no Kenwood reference calibrates RM",
				*u.Patch.SWRRatio)
		}
	})

	t.Run("ALC", func(t *testing.T) {
		u, err := k.Decode([]byte("RM30008"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.ALC == nil || u.Patch.ALC.Raw != 8 {
			t.Errorf("ALC = %+v, want raw 8", u.Patch.ALC)
		}
	})

	// COMP has nowhere to go in State, but the frame still has to complete its
	// transaction: the three answers arrive in whatever order the rig sends
	// them, and any one of them may be what a read is waiting on.
	t.Run("COMP completes its transaction and publishes nothing", func(t *testing.T) {
		u, err := k.Decode([]byte("RM20020"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Key != keyRM {
			t.Errorf("key = %q, want %q so the pending read is answered", u.Key, keyRM)
		}
		if !u.Patch.Empty() {
			t.Errorf("patch = %+v, want nothing published for COMP", u.Patch)
		}
	})
}

// RM is asked for only while transmitting: in receive SWR and ALC read zero and
// would cost a transaction a tick to say nothing.
func TestRMPolledOnlyWhileTransmitting(t *testing.T) {
	// The IF answer decoded at the top of the same poll is what sets the flag,
	// so the fixture has to say the rig is keyed rather than the test poking
	// the field: that is the path a real transmission takes.
	asked := func(t *testing.T, transmitting bool) bool {
		t.Helper()
		k := newRig(t, 2, true)
		answers := initAnswers()
		if transmitting {
			f := []byte(sampleIF)
			f[ifTX] = '1'
			answers[reqIF] = string(f)
			answers[reqRM] = "RM10012"
		}
		c := newTestConn(t, k, answers)
		if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		for _, s := range c.sent {
			if s == reqRM {
				return true
			}
		}
		return false
	}

	if asked(t, false) {
		t.Error("polled RM in receive")
	}
	if !asked(t, true) {
		t.Error("did not poll RM while transmitting")
	}
}
