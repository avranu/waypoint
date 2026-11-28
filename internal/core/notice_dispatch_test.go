package core

import (
	"strings"
	"testing"
	"time"
)

func TestPrivateEventWithoutHintIsDispatchedQuietly(t *testing.T) {
	e := newEnv(t)
	sub := e.s.Hub.Subscribe(e.admin.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	// Addressed to the subscriber, so the hub would send it if it got it.
	e.s.dispatch(e.ctx, e.admin, []*Event{{Kind: "test.private", Private: true, UserID: e.admin.User.ID}})
	select {
	case ev := <-sub.C:
		t.Errorf("a hub subscriber got the private %s event", ev.Kind)
	default:
	}
}

func TestStoredNoticeIsDeliveredAndItsEventIsNotPublished(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	sub := e.s.Hub.Subscribe(sam.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	err := e.s.write(e.ctx, e.admin, func(t *tx) error {
		_, err := t.storeNotice(e.ctx, sam.User.ID, NotifyMentioned, "Stored", "body", "/x")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case ev := <-sub.C:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == EvNotification {
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	got := strings.Join(kinds, ",")
	if !strings.Contains(got, EvNotification) {
		t.Errorf("the stored notice was not delivered live: %q", got)
	}
	for _, k := range kinds {
		if k == evNotificationStored {
			t.Errorf("the %s event reached a hub subscriber", k)
		}
	}
	// Anything else the hub has queued must not be the private event either.
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == evNotificationStored {
				t.Errorf("the %s event reached a hub subscriber", ev.Kind)
			}
			continue
		default:
		}
		break
	}
}
