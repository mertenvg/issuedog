package github

import (
	"context"
	"errors"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// standingEvent is a label change on the timeline, by who.
func standingEvent(id int64, event, who, label string) *gogithub.Timeline {
	typ := "User"
	if who == appBot {
		typ = "Bot"
	}
	return &gogithub.Timeline{ID: &id, Event: &event, Actor: &gogithub.User{Login: &who, Type: &typ}, Label: &gogithub.Label{Name: label}}
}

// standingFailTrust fails on every login.
type standingFailTrust struct{}

var errStandingTrust = errors.New("trust is down")

func (standingFailTrust) Trusted(context.Context, string) (bool, error) {
	return false, errStandingTrust
}

func TestStandingLabelChange(t *testing.T) {
	tests := []struct {
		name     string
		timeline []*gogithub.Timeline
		want     int64 // the ID of the change that stands, or 0
	}{
		{"none", nil, 0},
		{"the owner's add", []*gogithub.Timeline{standingEvent(1, "labeled", "owner", "type:epic")}, 1},
		{"the App's add, any case", []*gogithub.Timeline{standingEvent(1, "labeled", appBot, "Type:Epic")}, 1},
		{"an untrusted add", []*gogithub.Timeline{standingEvent(1, "labeled", "mallory", "type:epic")}, 0},
		{"another label", []*gogithub.Timeline{standingEvent(1, "labeled", "owner", "type:story")}, 0},
		{"the App's restore of the owner's add", []*gogithub.Timeline{
			standingEvent(1, "labeled", "owner", "type:epic"),
			standingEvent(2, "unlabeled", "mallory", "type:epic"),
			standingEvent(3, "labeled", appBot, "type:epic"),
		}, 1},
		{"the App's restore of its own add", []*gogithub.Timeline{
			standingEvent(1, "labeled", appBot, "type:epic"),
			standingEvent(2, "unlabeled", "mallory", "type:epic"),
			standingEvent(3, "labeled", appBot, "type:epic"),
		}, 1},
		{"the App's revert of an untrusted add", []*gogithub.Timeline{
			standingEvent(1, "labeled", "mallory", "type:epic"),
			standingEvent(2, "unlabeled", appBot, "type:epic"),
		}, 0},
		{"the owner's removal", []*gogithub.Timeline{
			standingEvent(1, "labeled", appBot, "type:epic"),
			standingEvent(2, "unlabeled", "owner", "type:epic"),
		}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StandingLabelChange(context.Background(), tt.timeline, "type:epic", "issuedog", fakeTrust{"owner": true})
			if err != nil {
				t.Fatal(err)
			}
			if got.GetID() != tt.want {
				t.Errorf("StandingLabelChange = %d, want %d", got.GetID(), tt.want)
			}
		})
	}
	_, err := StandingLabelChange(context.Background(), []*gogithub.Timeline{standingEvent(1, "labeled", "owner", "type:epic")},
		"type:epic", "issuedog", standingFailTrust{})
	if !errors.Is(err, errStandingTrust) {
		t.Errorf("StandingLabelChange with trust failing = %v, want its error", err)
	}
}
