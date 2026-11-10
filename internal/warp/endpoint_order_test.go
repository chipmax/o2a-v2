package warp

import (
	"reflect"
	"testing"
)

func TestOrderEndpoints(t *testing.T) {
	endpoints := []string{
		"[2606:4700:d0::1]:2408",
		"162.159.192.9:2408",
		"[2606:4700:d0::2]:500",
		"162.159.192.9:500",
	}
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "ipv6 first",
			got:  preferIPv6Endpoints(endpoints),
			want: []string{
				"[2606:4700:d0::1]:2408",
				"[2606:4700:d0::2]:500",
				"162.159.192.9:2408",
				"162.159.192.9:500",
			},
		},
		{
			name: "ipv4 first",
			got:  preferIPv4Endpoints(endpoints),
			want: []string{
				"162.159.192.9:2408",
				"162.159.192.9:500",
				"[2606:4700:d0::1]:2408",
				"[2606:4700:d0::2]:500",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("order = %v, want %v", test.got, test.want)
			}
		})
	}
}
