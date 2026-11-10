package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeNewTimerNonPositiveFiresWithoutAdvance(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
	}{{"zero", 0}, {"negative", -time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(100, 0)
			f := NewFake(start)
			timer := f.NewTimer(tc.d)
			select {
			case got := <-timer.C():
				if !got.Equal(start) {
					t.Fatalf("fire time = %s, want %s", got, start)
				}
			default:
				t.Fatal("non-positive timer did not fire without Advance")
			}
			if timer.Stop() || f.Pending() != 0 {
				t.Fatal("already-fired timer remained active or could be stopped")
			}
			f.Advance(time.Second)
			select {
			case <-timer.C():
				t.Fatal("one-shot timer fired twice")
			default:
			}
		})
	}
}

func TestFakeTimerStopBeforeAndAfterFire(t *testing.T) {
	f := NewFake(time.Unix(100, 0))
	stopped := f.NewTimer(time.Second)
	if f.Pending() != 1 || !stopped.Stop() || stopped.Stop() || f.Pending() != 0 {
		t.Fatal("Stop before expiry did not cancel exactly once")
	}
	f.Advance(time.Second)
	select {
	case <-stopped.C():
		t.Fatal("stopped timer fired")
	default:
	}
	fired := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if fired.Stop() || f.Pending() != 0 {
		t.Fatal("Stop after expiry succeeded or timer remained pending")
	}
	select {
	case <-fired.C():
	default:
		t.Fatal("expired timer lost its buffered signal")
	}
}

func TestFakeTimerStopRacesAdvance(t *testing.T) {
	for range 100 {
		f := NewFake(time.Unix(100, 0))
		timer := f.NewTimer(time.Second)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var stopped bool
		wg.Add(2)
		go func() { defer wg.Done(); <-start; stopped = timer.Stop() }()
		go func() { defer wg.Done(); <-start; f.Advance(time.Second) }()
		close(start)
		wg.Wait()
		select {
		case <-timer.C():
			if stopped {
				t.Fatal("successful Stop raced into a signal")
			}
		default:
			if !stopped {
				t.Fatal("failed Stop without expiry signal")
			}
		}
		if timer.Stop() || f.Pending() != 0 {
			t.Fatal("settled timer remained active")
		}
	}
}

// Registration and Advance may win in either order. A positive duration is
// relative to registration, not to a caller's earlier Now sample; don't silently
// change this contract to compensate for a stale duration in a test harness.
func TestFakeNewTimerRacesAdvance(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 5 * time.Second} {
		for range 100 {
			start := time.Unix(100, 0)
			f := NewFake(start)
			gate := make(chan struct{})
			var timer Timer
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-gate; timer = f.NewTimer(d) }()
			go func() { defer wg.Done(); <-gate; f.Advance(time.Second) }()
			close(gate)
			wg.Wait()
			if d > 0 {
				select {
				case <-timer.C():
					t.Fatal("positive timer fired before its registered duration")
				default:
				}
				if f.Pending() != 1 {
					t.Fatal("positive timer was not active")
				}
				f.Advance(d)
			}
			select {
			case got := <-timer.C():
				if got.Before(start) || got.After(f.Now()) {
					t.Fatalf("invalid fire timestamp %s", got)
				}
			default:
				t.Fatal("timer lost its signal across registration/Advance")
			}
			if timer.Stop() || f.Pending() != 0 {
				t.Fatal("fired timer remained active")
			}
		}
	}
}

// Absolute registration uses the supplied time even if Now advanced since it
// was sampled. Immediate signals require no extra Advance to become visible.
func TestFakeNewTimerAt(t *testing.T) {
	start := time.Unix(100, 0)
	for _, tc := range []struct {
		name   string
		target time.Time
		future bool
	}{
		{"future", start.Add(5 * time.Second), true},
		{"equal_now", start, false},
		{"past", start.Add(-time.Second), false},
		{"zero_time", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake(start)
			timer := f.NewTimerAt(tc.target)
			if tc.future {
				if f.Pending() != 1 {
					t.Fatal("future absolute timer is not active")
				}
				f.Advance(4 * time.Second)
				select {
				case <-timer.C():
					t.Fatal("absolute timer fired before target")
				default:
				}
				f.Advance(time.Second)
			}
			select {
			case got := <-timer.C():
				if !got.Equal(f.Now()) {
					t.Fatalf("fire time = %s, want %s", got, f.Now())
				}
			default:
				t.Fatal("absolute timer did not deliver at/past target")
			}
			if timer.Stop() || f.Pending() != 0 {
				t.Fatal("fired absolute timer remained active")
			}
			f.Advance(10 * time.Second)
			select {
			case <-timer.C():
				t.Fatal("absolute one-shot fired twice")
			default:
			}
		})
	}
}

func TestFakeNewTimerAtStop(t *testing.T) {
	f := NewFake(time.Unix(100, 0))
	timer := f.NewTimerAt(f.Now().Add(time.Second))
	if !timer.Stop() || timer.Stop() || f.Pending() != 0 {
		t.Fatal("future absolute Stop did not cancel exactly once")
	}
	f.Advance(time.Second)
	select {
	case <-timer.C():
		t.Fatal("stopped absolute timer fired")
	default:
	}
}

func TestFakeNewTimerAtRacesAdvance(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, 0, time.Second} {
		for range 100 {
			start := time.Unix(100, 0)
			f := NewFake(start)
			gate := make(chan struct{})
			var timer Timer
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-gate; timer = f.NewTimerAt(start.Add(offset)) }()
			go func() { defer wg.Done(); <-gate; f.Advance(time.Second) }()
			close(gate)
			wg.Wait()
			select {
			case got := <-timer.C():
				if got.Before(start) || got.After(f.Now()) {
					t.Fatalf("invalid fire timestamp %s", got)
				}
			default:
				t.Fatal("absolute registration drifted across Advance")
			}
			if timer.Stop() || f.Pending() != 0 {
				t.Fatal("settled absolute timer remained active")
			}
			select {
			case <-timer.C():
				t.Fatal("absolute timer fired twice during registration race")
			default:
			}
		}
	}
}

func TestFakeNewTimerAtStopRacesAdvance(t *testing.T) {
	for range 100 {
		f := NewFake(time.Unix(100, 0))
		timer := f.NewTimerAt(f.Now().Add(time.Second))
		gate := make(chan struct{})
		var wg sync.WaitGroup
		var stopped bool
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; stopped = timer.Stop() }()
		go func() { defer wg.Done(); <-gate; f.Advance(time.Second) }()
		close(gate)
		wg.Wait()
		select {
		case <-timer.C():
			if stopped {
				t.Fatal("successful absolute Stop raced into delivery")
			}
		default:
			if !stopped {
				t.Fatal("failed absolute Stop without delivery")
			}
		}
		if timer.Stop() || f.Pending() != 0 {
			t.Fatal("absolute Stop race left an active timer")
		}
	}
}

func TestRealNewTimerAt(t *testing.T) {
	for _, target := range []time.Time{time.Time{}, time.Now().Add(-time.Second)} {
		timer := (Real{}).NewTimerAt(target)
		select {
		case <-timer.C():
		case <-time.After(time.Second):
			t.Fatal("past real absolute timer did not wake")
		}
		if timer.Stop() {
			t.Fatal("consumed real timer could still be stopped")
		}
	}
	timer := (Real{}).NewTimerAt(time.Now().Add(time.Hour))
	if !timer.Stop() || timer.Stop() {
		t.Fatal("future real absolute timer could not be cancelled exactly once")
	}
}
