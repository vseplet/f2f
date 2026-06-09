package dns

import (
	"testing"
	"time"

	"github.com/miekg/dns"
)

type stubResolver struct{}

func (stubResolver) LookupHost(label string) (Host, bool) {
	if label == "gitea" {
		return Host{V4: "100.64.0.2"}, true
	}
	return Host{}, false
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return resp
}

// TestPinnedDomains drives the out-of-zone path: intercept domains
// pinned to exit-peer-resolved IPs answer A records, AAAA comes back
// empty NOERROR, unknown names are NXDOMAIN, and the camp zone still
// resolves via the zone handler.
func TestPinnedDomains(t *testing.T) {
	pins := map[string][]string{"work-vpn.ru": {"10.8.3.7", "10.8.3.8"}}
	srv, err := Open("127.0.0.1:0", "testcamp", stubResolver{}, func(name string) []string {
		return pins[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	resp := query(t, srv.Addr(), "work-vpn.ru", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 2 {
		t.Fatalf("pinned A: rcode=%v answers=%d, want NOERROR/2", resp.Rcode, len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || a.A.String() != "10.8.3.7" {
		t.Fatalf("pinned A answer = %v, want 10.8.3.7", resp.Answer[0])
	}

	resp = query(t, srv.Addr(), "work-vpn.ru", dns.TypeAAAA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("pinned AAAA: rcode=%v answers=%d, want NOERROR/0", resp.Rcode, len(resp.Answer))
	}

	resp = query(t, srv.Addr(), "unknown.ru", dns.TypeA)
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("unknown name rcode = %v, want NXDOMAIN", resp.Rcode)
	}

	resp = query(t, srv.Addr(), "gitea.testcamp.f2f", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("zone A: rcode=%v answers=%d, want NOERROR/1", resp.Rcode, len(resp.Answer))
	}
}
