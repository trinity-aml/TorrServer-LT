package dlna

import (
	"testing"

	"github.com/anacrolix/log"
)

type capture struct{ recs []log.Record }

func (c *capture) Handle(r log.Record) { c.recs = append(c.recs, r) }

func rec(text string, names ...string) log.Record {
	return log.Record{Msg: log.Str(text), Names: names}
}

func TestSSDPNoiseFilter(t *testing.T) {
	cases := []struct {
		name string
		r    log.Record
		drop bool
	}{
		{"ssdp EOF dropped", rec("EOF", "dlna", "dms", "server", "ssdp", "eth0"), true},
		{"ssdp unexpected EOF dropped", rec("unexpected EOF", "dlna", "dms", "server", "ssdp", "wlan0"), true},
		{"ssdp real error kept", rec("error creating ssdp server on eth0: no route", "dlna", "dms", "server", "ssdp", "eth0"), false},
		{"ssdp started kept", rec("started SSDP on \"eth0\"", "dlna", "dms", "server", "ssdp", "eth0"), false},
		{"non-ssdp EOF kept", rec("EOF", "dlna", "dms", "server", "eventing"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &capture{}
			f := ssdpNoiseFilter{next: []log.Handler{sink}}
			f.Handle(c.r)
			got := len(sink.recs)
			if c.drop && got != 0 {
				t.Fatalf("expected record dropped, but handler saw %d", got)
			}
			if !c.drop && got != 1 {
				t.Fatalf("expected record forwarded once, handler saw %d", got)
			}
		})
	}
}
