package repository

import (
	"reflect"
	"testing"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

func TestStatesAtOrBelow(t *testing.T) {
	cases := []struct {
		state string
		want  []string
	}{
		{mongodb.MessageStateQueued, []string{mongodb.MessageStateQueued}},
		{mongodb.MessageStateDispatched, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched}},
		{mongodb.MessageStateAcknowledged, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateAcknowledged}},
		{"bogus", nil},
	}
	for _, c := range cases {
		got := statesAtOrBelow(c.state)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("statesAtOrBelow(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}
