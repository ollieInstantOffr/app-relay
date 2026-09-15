package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type fakeAdminListener struct {
	busy     int
	switched []int
}

func (f *fakeAdminListener) Ports() []int { return nil }

func (f *fakeAdminListener) CheckPort(port int) error {
	if port == f.busy {
		return errors.New("Port 9999 is already in use by another program")
	}
	return nil
}

func (f *fakeAdminListener) SwitchPort(_ context.Context, port int) error {
	f.switched = append(f.switched, port)
	return nil
}

func TestGeneralSettingsAdminPortMovesListener(t *testing.T) {
	s, _ := newTestService(t)
	fake := &fakeAdminListener{busy: 9999}
	s.app.AdminListener = fake
	req := httptest.NewRequest("PUT", "/api/settings/general", nil)
	prev := store.DefaultGeneral()

	busy := prev
	busy.AdminPort = 9999
	var ve *model.ValidationError
	if err := s.generalBeforeSave(req, &prev, &busy); !errors.As(err, &ve) || !strings.Contains(ve.Fields["adminPort"], "already in use") {
		t.Fatalf("busy port: err = %v", err)
	}

	next := prev
	next.AdminPort = 8154
	if err := s.generalBeforeSave(req, &prev, &next); err != nil {
		t.Fatalf("free port: %v", err)
	}
	s.generalAfterSave(req, &prev, &next)
	if len(fake.switched) != 1 || fake.switched[0] != 8154 {
		t.Fatalf("switched = %v, want [8154]", fake.switched)
	}

	// Saving other fields leaves the listener alone.
	other := next
	other.InstanceName = "Home"
	s.generalAfterSave(req, &next, &other)
	if len(fake.switched) != 1 {
		t.Fatalf("switched again on unrelated change: %v", fake.switched)
	}
}
