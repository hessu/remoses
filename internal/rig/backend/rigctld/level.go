package rigctld

import (
	"fmt"
	"math"

	"github.com/hessu/remoses/internal/radio"
)

// --- Power (RFPOWER) --------------------------------------------------------

// powerFromLevel turns Hamlib's RFPOWER reading into radio.Power.
//
// rig.h defines RFPOWER as "RF Power, arg float [0.0 ... 1.0]". It is a
// normalised setting, not watts, and Hamlib deliberately says nothing about
// what fraction of the rig's output any given value produces — that mapping is
// what the separate power2mW/mW2power calls exist for. So Watts stays nil and
// Caps reports PowerWattAccurate false; inventing a watt figure by multiplying
// by a nameplate maximum would be a lie that clients would then display to two
// decimal places.
//
// Native has no natural value here either: the wire carries a float, and
// radio.Power.Native is an int. It gets the percentage rounded, which is the
// closest thing to "the number that was on the wire" that fits, and it is
// documented as such rather than left at a misleading zero.
func powerFromLevel(v float64) radio.Power {
	if math.IsNaN(v) {
		v = 0
	}
	pct := min(max(v*100, 0), 100)
	return radio.Power{
		Watts:  nil,
		Pct:    pct,
		Native: int(math.Round(pct)),
	}
}

// levelFromPowerSet resolves a power request to the 0.0..1.0 float L RFPOWER
// takes.
//
// A watt request is refused rather than converted. Caps says the scale is not
// watt-accurate, so a caller asking for watts has either ignored that or is
// talking to the wrong radio, and quietly treating "40 watts" as "40 per cent"
// would be the worst of the available answers.
func levelFromPowerSet(p radio.PowerSet) (float64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if p.Watts != nil {
		return 0, fmt.Errorf("rigctld: Hamlib's RFPOWER is a 0..1 fraction with no watt meaning, " +
			"so power must be given as a percentage")
	}
	pct := *p.Pct
	if math.IsNaN(pct) {
		return 0, fmt.Errorf("rigctld: power request is not a number")
	}
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("rigctld: power %.1f%% is outside 0..100", pct)
	}
	return pct / 100, nil
}

// --- Transmit meters --------------------------------------------------------

// fractionScale is the full-scale Raw value for a meter that arrives as a 0..1
// fraction. 100 makes Raw a percentage, which is what the underlying number
// already means for RFPOWER_METER.
const fractionScale = 100

// meterFromFraction turns a 0..1 level into a percentage meter, clamped.
func meterFromFraction(v float64) radio.Meter {
	raw := int(v*fractionScale + 0.5)
	return radio.Meter{Raw: min(max(raw, 0), fractionScale), Scale: fractionScale}
}

// swrBarTop is where the SWR bar reads full scale, in tenths of a ratio: 3.0:1.
//
// A bar needs a top and an SWR has none — the level is documented as running to
// infinity — so one has to be chosen. 3.0 is not arbitrary: it is the highest
// point Icom's own SWR meter calibration names, which makes this backend's bar
// read like the ones next to it, and it is comfortably past the point where an
// operator should already have stopped transmitting.
//
// The exact ratio is published alongside in State.SWRRatio, so nothing is lost
// by the bar pinning.
const swrBarTop = 30

// meterFromSWR draws a ratio as a bar, pinned at swrBarTop.
func meterFromSWR(ratio float64) radio.Meter {
	raw := int(ratio*10 + 0.5)
	return radio.Meter{Raw: min(max(raw, 0), swrBarTop), Scale: swrBarTop}
}

// --- S-meter (STRENGTH) -----------------------------------------------------

// The STRENGTH level is the one meter reading in remoses that is genuinely
// calibrated. rig.h calls it "Effective (calibrated) signal strength relative to
// S9, arg int (dB)": the rig's backend has already run the raw meter through
// its own calibration table, so unlike the Icom 0..255 and the Kenwood 0..30
// dot count, the number means something in dB.
const (
	// sMeterS0DB is the reading at S0, nine S-units below S9 at the
	// conventional 6 dB per unit. It is the offset that turns a signed dB
	// figure into a Raw value a bar can be drawn from.
	sMeterS0DB = 54
	// sMeterMaxOverDB is the top of the bar, S9+60, which is as far as any
	// amateur S-meter is marked.
	sMeterMaxOverDB = 60
	// sMeterScale is the full-scale Raw value: S0 at 0, S9 at 54, S9+60 at 114.
	sMeterScale = sMeterS0DB + sMeterMaxOverDB
	// dbPerSUnit is the conventional spacing of the S-units below S9.
	dbPerSUnit = 6
)

// meterFromStrength converts dB relative to S9 into radio.Meter.
//
// S is the S-unit number: 9 at 0 dB, dropping one unit per 6 dB below, and
// continuing linearly above so that no reading is thrown away — S 12.5 is S9+21
// dB. Clients that want the conventional "S9+21" rendering can recover the dB
// exactly as (S-9)*6, which is why the extension is linear rather than clamped
// at 9.
//
// Raw and Scale exist for radio.Meter.Fraction, which draws a bar: Raw is the
// reading shifted so that S0 sits at zero, and Scale puts full deflection at
// S9+60.
func meterFromStrength(db int) radio.Meter {
	s := 9 + float64(db)/dbPerSUnit
	if s < 0 {
		s = 0
	}
	raw := db + sMeterS0DB
	return radio.Meter{
		Raw:   min(max(raw, 0), sMeterScale),
		Scale: sMeterScale,
		S:     &s,
	}
}

// --- Keyer speed (KEYSPD) ---------------------------------------------------

// Bounds for the keyer speed when the daemon does not supply its own.
//
// Hamlib has no universal KEYSPD range: level_gran comes from each rig's
// backend and many leave it zero. These match the range the shared pacing layer
// in internal/cw will key at, and are replaced by the rig's own figures
// whenever \dump_state carries a usable level_gran. The constants are duplicated
// rather than imported because internal/cw depends on this package's interface,
// not the other way round.
const (
	defaultMinWPM = 5
	defaultMaxWPM = 60
)
