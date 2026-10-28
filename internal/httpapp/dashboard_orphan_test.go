package httpapp

import (
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/store"
)

func TestUnlinkedOutboundEventsStayOutOfRecentMessages(t *testing.T) {
	now := time.Now().UTC()
	entries := []store.DomainLogEntry{
		{ID: "393", Kind: "sending", Status: "sending", Subject: "Historical orphan", Provider: "brevo", At: now},
		{ID: "394", Kind: "sent", Status: "sent", Subject: "Historical orphan", Provider: "brevo", At: now.Add(time.Second)},
		{ID: "395", Kind: "sent", Status: "sent", Subject: "Real message", Provider: "brevo", MessageID: "msg_real", DeliveryKey: "msg_real", At: now.Add(2 * time.Second)},
		{ID: "396", Kind: "received", Status: "received", Subject: "Inbound without link", At: now.Add(3 * time.Second)},
	}
	messages := dashboardActivityRows(entries)
	if len(messages) != 2 {
		t.Fatalf("recent messages has %d rows, want linked outbound plus inbound: %+v", len(messages), messages)
	}
	if messages[0].ID != "msg_real" || messages[1].Subject != "Inbound without link" {
		t.Fatalf("wrong recent messages: %+v", messages)
	}
	events := unlinkedDeliveryEvents(entries)
	if len(events) != 2 || events[0].ID != "393" || events[1].ID != "394" {
		t.Fatalf("missing/merged delivery audit events: %+v", events)
	}
}
