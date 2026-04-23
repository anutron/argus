package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/drn/argus/internal/testutil"
)

func TestLazyScreen_ClearDelegates(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	sim.Init()
	defer sim.Fini()
	sim.SetSize(10, 5)

	ls := &lazyScreen{Screen: sim}

	// Write a character, then Clear — must erase it. lazyScreen is a pure
	// passthrough; any skip behavior is a regression of the ghosting fix.
	sim.SetContent(0, 0, 'X', nil, tcell.StyleDefault)
	ls.Clear()
	str, _, _ := sim.Get(0, 0)
	testutil.Equal(t, str, " ")
}
