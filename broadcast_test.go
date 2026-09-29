package events

import (
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func runNATS(t *testing.T) *nats.Conn {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

type recorder struct {
	mu   sync.Mutex
	msgs []BroadcastMessage
}

func (r *recorder) handle(m BroadcastMessage) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// Two Broadcasters on separate connections stand in for two pods of one service.
func TestBroadcasterFansOutOncePerPod(t *testing.T) {
	podA := NewBroadcaster(nil, runNATS(t), "pos")
	srvConn := podA.conn
	connB, err := nats.Connect(srvConn.ConnectedUrl())
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()
	podB := NewBroadcaster(nil, connB, "pos")

	var recA, recB recorder
	if err := podA.Subscribe("notif", recA.handle); err != nil {
		t.Fatal(err)
	}
	if err := podB.Subscribe("notif", recB.handle); err != nil {
		t.Fatal(err)
	}
	_ = srvConn.Flush()
	_ = connB.Flush()

	tenant := "3f0e7a2c-1111-4c3a-9a55-2f6d7c1b0e01"
	if err := podA.Publish("notif", tenant, "user:42", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for recB.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // give any duplicate echo time to arrive

	if recA.count() != 1 {
		t.Fatalf("publishing pod got %d deliveries, want exactly 1 (no echo)", recA.count())
	}
	if recB.count() != 1 {
		t.Fatalf("other pod got %d deliveries, want 1", recB.count())
	}
	got := recB.msgs[0]
	if got.TenantID != tenant || got.Scope != "user:42" || string(got.Data) != `{"x":1}` {
		t.Fatalf("unexpected message %+v", got)
	}
}

func TestBroadcasterEmptyScopeAndLocalOnly(t *testing.T) {
	local := NewBroadcaster(nil, nil, "inventory")
	var rec recorder
	_ = local.Subscribe("cache.invalidate", rec.handle)
	if err := local.Publish("cache.invalidate", "", "", []byte("k")); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 || rec.msgs[0].TenantID != "" || rec.msgs[0].Scope != "" {
		t.Fatalf("local delivery wrong: %+v", rec.msgs)
	}
}

func TestSubjectTokenSanitizes(t *testing.T) {
	if got := subjectToken("a.b c*>"); got != "a_b_c__" {
		t.Fatalf("got %q", got)
	}
}
