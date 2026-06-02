//go:build darwin

package platform

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestRouteIfaceArgs(t *testing.T) {
	cases := []struct {
		name   string
		action string
		prefix string
		iface  string
		want   []string
	}{
		{
			name:   "ipv4 host route",
			action: "add",
			prefix: "1.2.3.4/32",
			iface:  "utun5",
			want:   []string{"-n", "add", "-inet", "-host", "1.2.3.4", "-interface", "utun5"},
		},
		{
			name:   "ipv4 cidr route",
			action: "add",
			prefix: "10.0.0.0/24",
			iface:  "utun5",
			want:   []string{"-n", "add", "-inet", "-net", "10.0.0.0/24", "-interface", "utun5"},
		},
		{
			name:   "ipv4 delete host",
			action: "delete",
			prefix: "8.8.8.8/32",
			iface:  "utun7",
			want:   []string{"-n", "delete", "-inet", "-host", "8.8.8.8", "-interface", "utun7"},
		},
		{
			name:   "ipv6 host route",
			action: "add",
			prefix: "2001:db8::1/128",
			iface:  "utun5",
			want:   []string{"-n", "add", "-inet6", "-host", "2001:db8::1", "-interface", "utun5"},
		},
		{
			name:   "ipv6 cidr route",
			action: "add",
			prefix: "2001:db8::/64",
			iface:  "utun5",
			want:   []string{"-n", "add", "-inet6", "-net", "2001:db8::/64", "-interface", "utun5"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := netip.ParsePrefix(c.prefix)
			if err != nil {
				t.Fatalf("parse prefix %q: %v", c.prefix, err)
			}
			got := routeIfaceArgs(c.action, p, c.iface)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got: %v\nwant: %v", got, c.want)
			}
		})
	}
}

func TestRouteRejectArgs(t *testing.T) {
	cases := []struct {
		name   string
		action string
		prefix string
		want   []string
	}{
		{
			name:   "ipv6 host reject add",
			action: "add",
			prefix: "2a02:6b8::/128",
			want:   []string{"-n", "add", "-inet6", "-host", "2a02:6b8::", "::1", "-reject"},
		},
		{
			name:   "ipv6 cidr reject add",
			action: "add",
			prefix: "2001:db8::/64",
			want:   []string{"-n", "add", "-inet6", "-net", "2001:db8::/64", "::1", "-reject"},
		},
		{
			name:   "ipv4 host reject add",
			action: "add",
			prefix: "1.2.3.4/32",
			want:   []string{"-n", "add", "-inet", "-host", "1.2.3.4", "127.0.0.1", "-reject"},
		},
		{
			name:   "ipv6 host reject delete",
			action: "delete",
			prefix: "2a02:6b8::/128",
			want:   []string{"-n", "delete", "-inet6", "-host", "2a02:6b8::"},
		},
		{
			name:   "ipv4 host reject delete",
			action: "delete",
			prefix: "1.2.3.4/32",
			want:   []string{"-n", "delete", "-inet", "-host", "1.2.3.4"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := netip.ParsePrefix(c.prefix)
			if err != nil {
				t.Fatalf("parse prefix %q: %v", c.prefix, err)
			}
			got := routeRejectArgs(c.action, p)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got: %v\nwant: %v", got, c.want)
			}
		})
	}
}
