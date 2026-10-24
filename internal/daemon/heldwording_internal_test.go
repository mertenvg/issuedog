package daemon

import (
	"fmt"
	"slices"
	"testing"
)

// #222: the held= record's text names each /hold by its comment and the
// issue its heldon= entry names, in order; one with no issue there, as in
// a record from before heldon=, or an ill-formed one, by its comment
// alone.
func TestHeldWordingHolds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		held, heldOn []string
		want         string
	}{
		{"one, with its issue", []string{"11"}, []string{"2"}, "11 on #2"},
		{"two, each with its issue", []string{"11", "12"}, []string{"2", "1"}, "11 on #2 and 12 on #1"},
		{"three", []string{"11", "12", "13"}, []string{"3", "2", "1"}, "11 on #3, 12 on #2 and 13 on #1"},
		{"no heldon", []string{"11", "12"}, nil, "11 and 12"},
		{"a short heldon names the first ones", []string{"11", "12"}, []string{"2"}, "11 on #2 and 12"},
		{"an empty or ill-formed issue names none", []string{"11", "12", "13"}, []string{"", "x", "0"}, "11, 12 and 13"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := heldWordingHolds(c.held, c.heldOn); got != c.want {
				t.Errorf("heldWordingHolds(%q, %q) = %q; want %q", c.held, c.heldOn, got, c.want)
			}
		})
	}
}

// #222: resumeCheck's reason names the issue the reply is posted on as
// "this issue", with "here", and each other issue by number, with
// "there"; with both, the others come first. The others take "is" for
// one and "are" for more (#389).
func TestHeldWordingDiscussed(t *testing.T) {
	t.Parallel()
	const (
		there = " on hold for discussion, which only a code owner's response there, or `/resume` there, ends"
		here  = "this issue is on hold for discussion, which only a code owner's response here, or `/resume` here, ends"
	)
	for _, c := range []struct {
		name      string
		discussed []string
		self      string
		want      string
	}{
		{"only self", []string{"#2"}, "#2", here},
		{"only another", []string{"#2"}, "#1", "#2 is" + there},
		{"only others", []string{"#3", "#4"}, "#1", "#3, #4 are" + there},
		{"others and self", []string{"#3", "#4", "#1"}, "#1", "#3, #4 are" + there + "; and " + here},
		{"self, then another", []string{"#1", "#3"}, "#1", "#3 is" + there + "; and " + here},
		{"a number that only starts like self's", []string{"#12"}, "#1", "#12 is" + there},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			discussed := append([]string(nil), c.discussed...)
			if got := heldWordingDiscussed(discussed, c.self); got != c.want {
				t.Errorf("heldWordingDiscussed(%q, %q) = %q; want %q", c.discussed, c.self, got, c.want)
			}
			if !slices.Equal(discussed, c.discussed) {
				t.Errorf("heldWordingDiscussed changed its argument to %q; want %q", discussed, c.discussed)
			}
		})
	}
}

// #389: resumeCheck's reason for owner holds left names the issue the
// reply is posted on, where some of them were made, as "this issue", with
// "here", and each other issue by number, with "there", as its reason for
// discussion holds does; with both, the others come first.
func TestHeldWordingMakers(t *testing.T) {
	t.Parallel()
	const (
		there = "the holds below were made on %s, by `/hold` or by hand, so `/resume` there ends them"
		here  = "the holds left were made on this issue, by `/hold` or by hand, so `/resume` here ends them"
		rest  = "; and the rest were made on this issue, so `/resume` here ends them"
	)
	for _, c := range []struct {
		name   string
		makers []string
		self   string
		want   string
	}{
		{"only self", []string{"#1"}, "#1", here},
		{"only another", []string{"#2"}, "#1", fmt.Sprintf(there, "#2")},
		{"only others", []string{"#2", "#3"}, "#1", fmt.Sprintf(there, "#2, #3")},
		{"others and self", []string{"#2", "#1", "#3"}, "#1", fmt.Sprintf(there, "#2, #3") + rest},
		{"self, then another", []string{"#1", "#2"}, "#1", fmt.Sprintf(there, "#2") + rest},
		{"a number that only starts like self's", []string{"#12"}, "#1", fmt.Sprintf(there, "#12")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			makers := append([]string(nil), c.makers...)
			if got := heldWordingMakers(makers, c.self); got != c.want {
				t.Errorf("heldWordingMakers(%q, %q) = %q; want %q", c.makers, c.self, got, c.want)
			}
			if !slices.Equal(makers, c.makers) {
				t.Errorf("heldWordingMakers changed its argument to %q; want %q", makers, c.makers)
			}
		})
	}
}
