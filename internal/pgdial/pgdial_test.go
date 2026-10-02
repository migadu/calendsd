package pgdial

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := Config("host=10.0.0.1,10.0.0.2,10.0.0.3 port=5433 dbname=sabredav user=u sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, DefaultConnectTimeout)
	}
	// One port applies to every host; the hosts after the first are fallbacks.
	// (sslmode=disable: prefer would list each host twice, TLS then plain.)
	got := []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}
	for _, fb := range cfg.Fallbacks {
		got = append(got, fmt.Sprintf("%s:%d", fb.Host, fb.Port))
	}
	want := []string{"10.0.0.1:5433", "10.0.0.2:5433", "10.0.0.3:5433"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
}

func TestConfigKeepsExplicitTimeout(t *testing.T) {
	cfg, err := Config("host=10.0.0.1 connect_timeout=12")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 12*time.Second {
		t.Errorf("ConnectTimeout = %v, want 12s", cfg.ConnectTimeout)
	}
}

// blackhole accepts TCP connections and never answers, like a host whose
// Postgres (or HAProxy) hangs. It reports each accepted connection on the
// returned channel.
func blackhole(t *testing.T) (port int, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan struct{}, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open until the listener closes
			ch <- struct{}{}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, ch
}

// A hung first host must cost one connect_timeout, then pgx moves on to the
// next host rather than failing the connection outright.
func TestHungHostFailsOverToNext(t *testing.T) {
	p1, acc1 := blackhole(t)
	p2, acc2 := blackhole(t)
	cfg, err := Config(fmt.Sprintf("host=127.0.0.1,127.0.0.1 port=%d,%d user=u dbname=d sslmode=disable connect_timeout=1", p1, p2))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		t.Fatal("connect to two hung hosts succeeded")
	}
	elapsed := time.Since(start)
	for i, acc := range []<-chan struct{}{acc1, acc2} {
		select {
		case <-acc:
		default:
			t.Errorf("host %d was never tried", i+1)
		}
	}
	// Two attempts at 1s each; well under the 10s outer deadline.
	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Errorf("elapsed %v, want ~2s (one connect_timeout per host)", elapsed)
	}
}
