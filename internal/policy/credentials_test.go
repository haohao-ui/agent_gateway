package policy

import (
	"testing"
	"time"
)

func TestCredentialMetadataAndLifetimeMigration(t *testing.T) {
	s := newFrozenStore(t)
	p, token := mustIssue(t, s, Admin, nil, testTime.Add(365*24*time.Hour))
	_, short := mustIssue(t, s, Viewer, []string{testNode}, testTime.Add(time.Hour))
	if err := s.LimitLifetime(t.Context(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	items, err := s.Credentials(t.Context(), "", 1)
	if err != nil || len(items) != 1 {
		t.Fatal("invalid page", err)
	}
	second, err := s.Credentials(t.Context(), items[0].ID, 1)
	if err != nil || len(second) != 1 || second[0].ID == items[0].ID {
		t.Fatal("invalid cursor", err)
	}
	all, err := s.Credentials(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range all {
		if item.ID == p.ID && !item.ExpiresAt.Equal(testTime.Add(24*time.Hour)) {
			t.Fatal("legacy lifetime not shortened")
		}
	}
	freezeClock(s, testTime.Add(25*time.Hour))
	if _, err := s.Authenticate(t.Context(), token); err == nil {
		t.Fatal("legacy token still live")
	}
	if _, err := s.Authenticate(t.Context(), short); err == nil {
		t.Fatal("short token extended")
	}
	if _, err := s.Credentials(t.Context(), "", 101); err == nil {
		t.Fatal("unbounded page")
	}
}
