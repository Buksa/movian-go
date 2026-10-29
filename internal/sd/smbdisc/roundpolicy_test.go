package smbdisc

import "testing"

func TestNoRoundAtStartOrOnTheClockWithoutAnOpenPage(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))

	for range 5 {
		r.clock.Advance(roundInterval)
	}
	lan.expectProbesStay(t, 0)
}

func TestOpeningThePageStartsARoundAtOnceThenEveryInterval(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))

	release := r.engine.Watch()
	defer release()
	lan.WaitProbes(1) // at once, the clock short of any tick
	r.clock.Advance(collectWindow)
	lan.expectProbesStay(t, 1)

	r.clock.Advance(roundInterval - collectWindow)
	lan.WaitProbes(2)
}

func TestClosingThePageStopsTheRounds(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))

	release := r.engine.Watch()
	lan.WaitProbes(1)
	r.clock.Advance(collectWindow)
	release()
	release() // idempotent
	r.clock.Advance(roundInterval)
	r.clock.Advance(roundInterval)
	lan.expectProbesStay(t, 1)
}

func TestANetworkChangeStartsARoundOnlyWhilePageIsOpen(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))

	r.network.change()
	r.clock.Advance(roundInterval)
	lan.expectProbesStay(t, 0)

	release := r.engine.Watch()
	defer release()
	lan.WaitProbes(1)
	r.clock.Advance(collectWindow)
	r.network.change()
	lan.WaitProbes(2)
}

func TestTwoOpenPagesKeepTheRoundsUntilBothClose(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))
	first := r.engine.Watch()
	lan.WaitProbes(1)
	r.clock.Advance(collectWindow)
	second := r.engine.Watch()
	lan.WaitProbes(2)
	r.clock.Advance(collectWindow)

	first()
	r.clock.Advance(roundInterval - collectWindow)
	lan.WaitProbes(3) // one page is still open
	second()
}

func TestAnEngineThatSentNothingAgesNoHost(t *testing.T) {
	lan := startTestLAN(t)
	lan.AddHost("127.0.0.2", "NAS/Workgroup:WORKGROUP")
	var down bool
	r := startEngineOn(t, lan, func() ([]lanInterface, error) {
		if down {
			return nil, nil // as when Android blocks the app's network
		}
		return loopback(t, "127.0.0.1"), nil
	})
	lan.WaitProbes(1)
	r.waitListing(entry{"smb/NAS", "NAS", "smb://127.0.0.2"})
	r.clock.Advance(collectWindow)
	lan.expectProbesStay(t, 1) // the first round is over, and has aged the hosts

	down = true
	for range 6 { // far beyond hostTTL
		r.clock.Advance(roundInterval)
	}
	lan.expectProbesStay(t, 1)
	r.waitListing(entry{"smb/NAS", "NAS", "smb://127.0.0.2"})
}

// A page that opens and closes before the engine hears of it leaves its
// opening signal behind; that signal starts no round.
func TestAStaleOpeningSignalStartsNoRound(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngineClosed(t, lan, interfaces(loopback(t, "127.0.0.1")...))

	r.engine.opened <- struct{}{} // the page closed again at once
	r.clock.Advance(roundInterval)
	lan.expectProbesStay(t, 0)

	release := r.engine.Watch()
	defer release()
	lan.WaitProbes(1) // a page that stays open still gets its round
}
