package syncplay

import (
	"math"
	"testing"
)

// decideDrift is the whole correction policy: silence, a quiet seek, a rate nudge or a full
// re-gate. It is pure, so each tier and boundary needs no room, socket or clock.

func TestDecideDriftLadder(t *testing.T) {
	const (
		soft   = 200.0   // SYNCPLAY_SOFT_SEEK_THRESHOLD_MS
		speed  = 10000.0 // SYNCPLAY_SPEED_SYNC_MAX_MS
		master = 100.0
		slow   = 0.8
		fast   = 1.5
	)

	cases := []struct {
		name        string
		peerTime    float64
		wantAction  driftAction
		wantRate    float64
		wantMSLower int64
		wantMSUpper int64
	}{
		{name: "in tolerance", peerTime: master + 0.05, wantAction: driftNone},
		{name: "exactly at the soft threshold", peerTime: master, wantAction: driftNone},
		{name: "soft correct", peerTime: master + 0.3, wantAction: driftSoftCorrect},
		{name: "ahead by 0.6s slows down", peerTime: master + 0.6, wantAction: driftSpeedSeek, wantRate: 0.8, wantMSLower: 1200, wantMSUpper: 1200},
		{name: "behind by 0.6s speeds up", peerTime: master - 0.6, wantAction: driftSpeedSeek, wantRate: 1.5, wantMSLower: 1200, wantMSUpper: 1200},
		{name: "behind by 9s is capped", peerTime: master - 9, wantAction: driftSpeedSeek, wantRate: 1.5, wantMSLower: 8000, wantMSUpper: 8000},
		{name: "behind by 10s re-gates", peerTime: master - 10, wantAction: driftRegate},
		{name: "wildly ahead re-gates", peerTime: master + 3600, wantAction: driftRegate},
	}

	for _, tc := range cases {
		action, rate, duration := decideDrift(tc.peerTime, master, soft, speed, slow, fast)
		if action != tc.wantAction {
			t.Errorf("%s: action = %d, want %d", tc.name, action, tc.wantAction)
			continue
		}
		if action == driftSpeedSeek {
			if rate != tc.wantRate {
				t.Errorf("%s: rate = %v, want %v", tc.name, rate, tc.wantRate)
			}
			if duration < tc.wantMSLower || duration > tc.wantMSUpper {
				t.Errorf("%s: duration = %d, want within [%d,%d]", tc.name, duration, tc.wantMSLower, tc.wantMSUpper)
			}
		}
	}
}

func TestDecideDriftUsesConfiguredRates(t *testing.T) {
	_, slow, _ := decideDrift(100.6, 100, 200, 10000, 0.91, 1.27)
	if slow != 0.91 {
		t.Fatalf("ahead correction rate = %v, want configured slowdown 0.91", slow)
	}
	_, fast, _ := decideDrift(99.4, 100, 200, 10000, 0.91, 1.27)
	if fast != 1.27 {
		t.Fatalf("behind correction rate = %v, want configured speedup 1.27", fast)
	}
}

func TestDecideDriftIsSymmetric(t *testing.T) {
	// Drift in either direction costs the same; only the corrective rate differs. Ninety-one
	// seconds off would already exceed the speed-sync window and re-gate instead.
	behind, behindRate, behindMS := decideDrift(91, 100, 200, 10000, 0.8, 1.5)
	ahead, aheadRate, aheadMS := decideDrift(109, 100, 200, 10000, 0.8, 1.5)

	if behind != ahead || behind != driftSpeedSeek {
		t.Fatalf("actions differ: %d vs %d", behind, ahead)
	}
	if behindMS != aheadMS {
		t.Errorf("durations differ: %d vs %d", behindMS, aheadMS)
	}
	if behindRate <= 1 || aheadRate >= 1 {
		t.Errorf("the peer behind must speed up (%v) and the one ahead slow down (%v)", behindRate, aheadRate)
	}
}

func TestDecideDriftHonoursConfiguredThresholds(t *testing.T) {
	// A deployment that widens the soft threshold must suppress corrections that the default
	// would have made, which is the point of the setting.
	if action, _, _ := decideDrift(100.3, 100, 200, 10000, 0.8, 1.5); action != driftSoftCorrect {
		t.Errorf("default thresholds: action = %d, want softCorrect", action)
	}
	if action, _, _ := decideDrift(100.3, 100, 500, 10000, 0.8, 1.5); action != driftNone {
		t.Errorf("widened soft threshold: action = %d, want none", action)
	}
	// Tightening the speed threshold turns a nudge into a re-gate.
	if action, _, _ := decideDrift(101, 100, 200, 500, 0.8, 1.5); action != driftRegate {
		t.Errorf("tightened speed threshold: action = %d, want re-gate", action)
	}
}

func TestDecideDriftDurationNeverExceedsCap(t *testing.T) {
	// 0.6s is the first drift in the speed-seek band; below it a soft correction carries no
	// duration. The cap bites at 4s, since duration is twice the drift.
	for seconds := 0.6; seconds <= 3.9; seconds += 0.1 {
		action, _, duration := decideDrift(100-seconds, 100, 200, 10000, 0.8, 1.5)
		if action != driftSpeedSeek {
			t.Fatalf("drift of %.1fs produced action %d, want a speed-seek", seconds, action)
		}
		if duration > maxSpeedSeekMS {
			t.Fatalf("drift of %.1fs produced a %dms speed-seek", seconds, duration)
		}
		if duration <= 0 {
			t.Fatalf("drift of %.1fs produced no duration", seconds)
		}
		if math.Abs(float64(duration)-seconds*2000) > 1 {
			t.Errorf("drift of %.1fs produced %dms, want roughly %.0fms", seconds, duration, seconds*2000)
		}
	}

	// Beyond the cap the duration stops growing, so a wild drift never plays at 1.5x for a
	// minute; the drift itself would re-gate before that anyway.
	for seconds := 4.0; seconds <= 9.9; seconds += 0.5 {
		_, _, duration := decideDrift(100-seconds, 100, 200, 10000, 0.8, 1.5)
		if duration != maxSpeedSeekMS {
			t.Errorf("drift of %.1fs produced %dms, want the %dms cap", seconds, duration, maxSpeedSeekMS)
		}
	}
}
