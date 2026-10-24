package domain

import (
	"errors"
	"testing"
)

func TestTransitions_AllPairs(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusCreated, StatusAssigned}:    true,
		{StatusCreated, StatusCancelled}:   true,
		{StatusAssigned, StatusInTransit}:  true,
		{StatusAssigned, StatusCancelled}:  true,
		{StatusInTransit, StatusCompleted}: true,
	}

	// Exhaustively check all 25 pairs so that adding a transition by accident
	// fails this test.
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			want := allowed[[2]Status{from, to}]
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s: CanTransitionTo = %v, want %v", from, to, got, want)
			}

			err := ValidateTransition(from, to)
			if want && err != nil {
				t.Errorf("%s -> %s: unexpected error %v", from, to, err)
			}
			if !want {
				var te *TransitionError
				if !errors.As(err, &te) || te.From != from || te.To != to {
					t.Errorf("%s -> %s: want *TransitionError, got %v", from, to, err)
				}
			}
		}
	}
}

func TestTerminalStatuses(t *testing.T) {
	terminal := map[Status]bool{StatusCompleted: true, StatusCancelled: true}
	for _, s := range AllStatuses {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}

	err := ValidateTransition(StatusCompleted, StatusCancelled)
	if err == nil || err.Error() != "trip is in terminal status COMPLETED and cannot transition to CANCELLED" {
		t.Errorf("terminal transition error message = %v", err)
	}
}

func TestSameStatusIsNotATransition(t *testing.T) {
	for _, s := range AllStatuses {
		if ValidateTransition(s, s) == nil {
			t.Errorf("%s -> %s should be rejected", s, s)
		}
	}
}

func TestParseStatus(t *testing.T) {
	for _, s := range AllStatuses {
		got, ok := ParseStatus(string(s))
		if !ok || got != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, ok)
		}
	}
	for _, bad := range []string{"", "assigned", "Assigned", " ASSIGNED", "DONE", "IN-TRANSIT"} {
		if _, ok := ParseStatus(bad); ok {
			t.Errorf("ParseStatus(%q) accepted an invalid status", bad)
		}
	}
}

func TestListFilterOffset(t *testing.T) {
	cases := []struct{ page, size, want int }{{1, 20, 0}, {2, 20, 20}, {3, 100, 200}}
	for _, c := range cases {
		if got := (ListFilter{Page: c.page, PageSize: c.size}).Offset(); got != c.want {
			t.Errorf("page %d size %d: offset %d, want %d", c.page, c.size, got, c.want)
		}
	}
}
